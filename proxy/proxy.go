package proxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"warp/route"
)

// handshakeTimeout bounds a SOCKS5 greeting/auth/request exchange. It is
// cleared once the tunnel is established: the connection is long-lived
// afterwards, and a leftover deadline would kill it mid-transfer.
const handshakeTimeout = 15 * time.Second

// maxAcceptBackoff bounds the exponential backoff for transient Accept errors.
const maxAcceptBackoff = time.Second

// Config controls the mixed proxy server.
type Config struct {
	// ListenAddr is the mixed proxy listen address (host:port).
	ListenAddr string

	// Username and Password, when both non-empty, make authentication
	// mandatory: RFC 1929 username/password for SOCKS5, HTTP Basic
	// (Proxy-Authorization) for HTTP.
	Username string
	Password string

	// AllowUDP enables SOCKS5 UDP ASSOCIATE. Datagrams are relayed from the
	// local stack and do NOT traverse the WARP tunnel — see udp.go.
	AllowUDP bool

	// Router, when non-nil, decides per-target whether it goes through the
	// tunnel ("proxy") or directly from the local machine ("direct"). host is
	// the bare target hostname (no port); ip is netip.Addr{} because routing
	// runs before any resolution. A nil Router keeps the original behavior:
	// everything through the tunnel.
	Router func(host string, ip netip.Addr) (action string, matched bool)

	// TunnelDial establishes a WARP tunnel byte stream to targetAddr. It is
	// used when Router selects "proxy" (or when Router is nil).
	TunnelDial func(ctx context.Context, targetAddr string) (net.Conn, error)
}

// Server is a mixed HTTP+SOCKS5 proxy on a single port. The first byte of a
// connection decides the protocol: 0x05 starts SOCKS5, anything else is HTTP.
type Server struct {
	cfg    Config
	mu     sync.Mutex
	ln     net.Listener
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewServer returns a server with the given configuration.
func NewServer(cfg Config) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{cfg: cfg, ctx: ctx, cancel: cancel}
}

// ListenAndServe binds cfg.ListenAddr and serves until Close.
func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("代理监听失败：%w", err)
	}
	return s.Serve(ln)
}

// Serve accepts connections on ln until Close. Accept errors are not all
// fatal: transient conditions (EMFILE, ECONNABORTED) back off and retry,
// mirroring main.go's accept loop.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	defer ln.Close()

	var backoff time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			if s.ctx.Err() != nil {
				return nil
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			if backoff == 0 {
				backoff = 5 * time.Millisecond
			} else if backoff *= 2; backoff > maxAcceptBackoff {
				backoff = maxAcceptBackoff
			}
			log.Printf("代理 Accept 出错：%v，%s 后重试", err, backoff)
			select {
			case <-time.After(backoff):
			case <-s.ctx.Done():
				return nil
			}
			continue
		}
		backoff = 0
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serveConn(conn)
		}()
	}
}

// Close stops the server: it cancels the context (closing every client
// connection via the per-connection watcher), closes the listener and waits
// for handlers to finish.
func (s *Server) Close() error {
	s.mu.Lock()
	ln := s.ln
	s.mu.Unlock()
	s.cancel()
	if ln != nil {
		_ = ln.Close()
	}
	s.wg.Wait()
	return nil
}

// serveConn sniffs the first byte and dispatches to the SOCKS5 or HTTP handler.
func (s *Server) serveConn(conn net.Conn) {
	defer conn.Close()

	// Monitor shutdown: closing conn unblocks any in-progress io.Copy relay
	// goroutines so serveConn returns promptly. The watcher stops when this
	// handler returns — s.ctx lives as long as the server, so waiting on it
	// alone would park one goroutine per connection for the whole run.
	handlerDone := make(chan struct{})
	defer close(handlerDone)
	go func() {
		select {
		case <-s.ctx.Done():
			conn.Close()
		case <-handlerDone:
		}
	}()

	br := bufio.NewReader(conn)
	first, err := br.Peek(1)
	if err != nil {
		return
	}
	if first[0] == 0x05 {
		s.handleSOCKS5(conn, br)
		return
	}
	s.handleHTTP(conn, br)
}

// handleSOCKS5 services one SOCKS5 connection (RFC 1928, optional RFC 1929).
func (s *Server) handleSOCKS5(conn net.Conn, br *bufio.Reader) {
	requireAuth := s.cfg.Username != "" && s.cfg.Password != ""
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))

	// 客户端问候：{VER=0x05, NMETHODS, METHODS...}
	var hdr [2]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return
	}
	if hdr[0] != 0x05 {
		return
	}
	methods := make([]byte, int(hdr[1]))
	if _, err := io.ReadFull(br, methods); err != nil {
		return
	}

	if requireAuth {
		// 只提供用户名/密码认证（RFC 1929，方法 0x02）。
		if _, err := conn.Write([]byte{0x05, 0x02}); err != nil {
			return
		}
		var authHdr [2]byte
		if _, err := io.ReadFull(br, authHdr[:]); err != nil {
			return
		}
		if authHdr[0] != 0x01 {
			return
		}
		user := make([]byte, int(authHdr[1]))
		if _, err := io.ReadFull(br, user); err != nil {
			return
		}
		var plen [1]byte
		if _, err := io.ReadFull(br, plen[:]); err != nil {
			return
		}
		passwd := make([]byte, int(plen[0]))
		if _, err := io.ReadFull(br, passwd); err != nil {
			return
		}
		if string(user) != s.cfg.Username || string(passwd) != s.cfg.Password {
			_, _ = conn.Write([]byte{0x01, 0x01}) // 认证失败
			return
		}
		if _, err := conn.Write([]byte{0x01, 0x00}); err != nil {
			return
		}
	} else {
		if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
			return
		}
	}

	// 请求：{VER, CMD, RSV, ATYP, DST.ADDR, DST.PORT}
	var reqHdr [4]byte
	if _, err := io.ReadFull(br, reqHdr[:]); err != nil {
		return
	}
	if reqHdr[0] != 0x05 {
		return
	}
	targetAddr, err := readSOCKS5Addr(br, reqHdr[3])
	if err != nil {
		sendErr(conn, 0x08)
		return
	}

	switch reqHdr[1] {
	case 0x01: // CONNECT
	case 0x03: // UDP ASSOCIATE
		if !s.cfg.AllowUDP {
			sendErr(conn, 0x07)
			return
		}
		_ = conn.SetDeadline(time.Time{})
		s.handleUDPAssociate(conn)
		return
	default:
		// BIND (0x02) 与其他命令不支持。
		sendErr(conn, 0x07)
		return
	}

	log.Printf("SOCKS5 CONNECT %s", targetAddr)
	_ = conn.SetDeadline(time.Time{})

	target, err := s.dial(s.ctx, targetAddr)
	if err != nil {
		log.Printf("SOCKS5 CONNECT %s 失败：%v", targetAddr, err)
		sendErr(conn, 0x04)
		return
	}
	sendSuccess(conn)
	log.Printf("SOCKS5 隧道已建立：%s", targetAddr)
	relay(s.ctx, conn, target)
}

// readSOCKS5Addr reads the ATYP + DST.ADDR + DST.PORT portion of a SOCKS5
// request from br and returns the canonical host:port.
func readSOCKS5Addr(br *bufio.Reader, addrType byte) (string, error) {
	var host string
	switch addrType {
	case 0x01: // IPv4
		var a [4]byte
		if _, err := io.ReadFull(br, a[:]); err != nil {
			return "", fmt.Errorf("IPv4 地址读取失败：%w", err)
		}
		host = net.IP(a[:]).String()
	case 0x03: // 域名
		var l [1]byte
		if _, err := io.ReadFull(br, l[:]); err != nil {
			return "", fmt.Errorf("域名长度读取失败：%w", err)
		}
		if l[0] == 0 {
			return "", fmt.Errorf("域名为空")
		}
		name := make([]byte, int(l[0]))
		if _, err := io.ReadFull(br, name); err != nil {
			return "", fmt.Errorf("域名读取失败：%w", err)
		}
		host = string(name)
	case 0x04: // IPv6
		var a [16]byte
		if _, err := io.ReadFull(br, a[:]); err != nil {
			return "", fmt.Errorf("IPv6 地址读取失败：%w", err)
		}
		host = net.IP(a[:]).String()
	default:
		return "", fmt.Errorf("未知的地址类型：%d", addrType)
	}

	var p [2]byte
	if _, err := io.ReadFull(br, p[:]); err != nil {
		return "", fmt.Errorf("端口读取失败：%w", err)
	}
	port := binary.BigEndian.Uint16(p[:])
	// 端口 0 不在此拒绝：UDP ASSOCIATE（RFC 1928 §7）的约定请求就是
	// DST.ADDR=0.0.0.0、DST.PORT=0（客户端尚未得知中继地址）。CONNECT 的
	// 端口 0 会在拨号阶段立即失败，同样安全。
	// JoinHostPort 而非 Sprintf：IPv6 字面量需要方括号，否则结果
	// （"2606:4700::1:443"）根本不是一个可解析的 host:port。
	return net.JoinHostPort(host, strconv.Itoa(int(port))), nil
}

// dial 按 Router 决策选择隧道或本地直连。与 tunnel 的 RouteFunc 语义一致：
// 只有显式命中 "proxy" 才进隧道；未命中（matched=false）按引擎的隐式 direct
// 兜底本地直连；Router 为 nil 时保持原行为——全部走隧道。
func (s *Server) dial(ctx context.Context, targetAddr string) (net.Conn, error) {
	if s.cfg.Router != nil {
		host, _, err := net.SplitHostPort(targetAddr)
		if err != nil {
			return nil, fmt.Errorf("目标地址 %q 无法解析为 host:port：%w", targetAddr, err)
		}
		action, matched := s.cfg.Router(host, netip.Addr{})
		if matched && action == route.ActionProxy {
			if s.cfg.TunnelDial == nil {
				return nil, fmt.Errorf("TunnelDial 未配置，无法建立 WARP 隧道")
			}
			return s.cfg.TunnelDial(ctx, targetAddr)
		}
		log.Printf("本地直连 %s", targetAddr)
		return (&net.Dialer{}).DialContext(ctx, "tcp", targetAddr)
	}
	if s.cfg.TunnelDial == nil {
		return nil, fmt.Errorf("TunnelDial 未配置，无法建立 WARP 隧道")
	}
	return s.cfg.TunnelDial(ctx, targetAddr)
}

// sendErr 发送 SOCKS5 错误响应（10 字节最小形式）。
func sendErr(conn net.Conn, code byte) {
	_, _ = conn.Write([]byte{0x05, code, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
}

// sendSuccess 发送 SOCKS5 成功响应。CONNECT 用全零 BND.ADDR 即可——与 tunnel
// 的 sendSocks5Success 一致。
func sendSuccess(conn net.Conn) {
	_, _ = conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
}

// relay 双向中继 client 与 target：任一侧结束就关闭两侧，让另一侧的 io.Copy
// 立即退出；ctx 取消时同样关闭两侧。target 必须是已建立的隧道或直连。
func relay(ctx context.Context, client, target net.Conn) {
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			_ = client.Close()
			_ = target.Close()
		})
	}
	defer closeBoth()

	watcherDone := make(chan struct{})
	defer close(watcherDone)
	go func() {
		select {
		case <-ctx.Done():
			closeBoth()
		case <-watcherDone:
		}
	}()

	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		_, _ = io.Copy(target, client)
		closeBoth()
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		_, _ = io.Copy(client, target)
		closeBoth()
	}()
	<-done
}

// handleHTTP services one HTTP connection (RFC 7230 forward proxy): CONNECT
// tunnels, anything else is forwarded as origin-form.
func (s *Server) handleHTTP(conn net.Conn, br *bufio.Reader) {
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}

	if !s.checkHTTPAuth(req) {
		_, _ = io.WriteString(conn, "HTTP/1.1 407 Proxy Authentication Required\r\n"+
			"Proxy-Authenticate: Basic realm=\"warp\"\r\nContent-Length: 0\r\n\r\n")
		return
	}

	if req.Method == http.MethodConnect {
		s.handleHTTPConnect(conn, req)
		return
	}
	s.handleHTTPForward(conn, br, req)
}

// handleHTTPConnect 处理 HTTP CONNECT（建立到目标的字节流隧道）。
func (s *Server) handleHTTPConnect(conn net.Conn, req *http.Request) {
	targetAddr := req.Host
	if _, _, err := net.SplitHostPort(targetAddr); err != nil {
		// CONNECT 请求省略端口时按 443 处理（RFC 7231 §4.3.6）。
		targetAddr = net.JoinHostPort(targetAddr, "443")
	}
	log.Printf("HTTP CONNECT %s", targetAddr)

	target, err := s.dial(s.ctx, targetAddr)
	if err != nil {
		log.Printf("HTTP CONNECT %s 失败：%v", targetAddr, err)
		_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		_ = target.Close()
		return
	}
	log.Printf("HTTP CONNECT 隧道已建立：%s", targetAddr)
	relay(s.ctx, conn, target)
}

// handleHTTPForward 转发非 CONNECT 请求：从 absolute-URI 改写为 origin-form
// 后原样发给目标。请求以 Connection: close 发出（逐跳头已剥离），响应结束
// 目标即关闭，中继随之拆除——不做 keep-alive 的多请求转发。
func (s *Server) handleHTTPForward(conn net.Conn, br *bufio.Reader, req *http.Request) {
	if req.URL == nil || req.URL.Host == "" {
		_, _ = io.WriteString(conn, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n")
		return
	}
	host, port := req.URL.Host, "80"
	if h, p, err := net.SplitHostPort(host); err == nil {
		host, port = h, p
	}
	targetAddr := net.JoinHostPort(host, port)
	log.Printf("HTTP %s %s（经 %s）", req.Method, req.URL.String(), targetAddr)

	target, err := s.dial(s.ctx, targetAddr)
	if err != nil {
		log.Printf("HTTP %s %s 失败：%v", req.Method, req.URL.String(), err)
		_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}

	stripHopByHop(req)
	req.RequestURI = ""
	req.URL.Scheme = ""
	req.URL.Host = ""
	req.Close = true

	if err := req.Write(target); err != nil {
		_ = target.Close()
		log.Printf("HTTP 转发 %s 失败：%v", targetAddr, err)
		_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	relay(s.ctx, conn, target)
}

// stripHopByHop 移除逐跳头：Proxy-Authorization 必须剥离（凭据不能泄露给
// 目标服务器），Connection 及其命名头、Proxy-Connection 同样不该转发。
func stripHopByHop(req *http.Request) {
	req.Header.Del("Proxy-Authorization")
	req.Header.Del("Proxy-Connection")
	for _, token := range req.Header.Values("Connection") {
		for _, name := range strings.Split(token, ",") {
			if name = strings.TrimSpace(name); name != "" {
				req.Header.Del(name)
			}
		}
	}
	req.Header.Del("Connection")
}

// checkHTTPAuth 校验 Proxy-Authorization: Basic。未配置认证时恒通过。
func (s *Server) checkHTTPAuth(req *http.Request) bool {
	if s.cfg.Username == "" && s.cfg.Password == "" {
		return true
	}
	h := req.Header.Get("Proxy-Authorization")
	if !strings.HasPrefix(h, "Basic ") {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, "Basic "))
	if err != nil {
		return false
	}
	user, pass, ok := strings.Cut(string(decoded), ":")
	return ok && user == s.cfg.Username && pass == s.cfg.Password
}

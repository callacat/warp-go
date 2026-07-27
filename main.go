package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"warp/registration"
	"warp/scanner"
	"warp/tunnel"
)

// defaultStateFile holds the registration: keys, token, edge endpoint and the
// edge public key that the connection is pinned to.
const defaultStateFile = "reg.json"

// usage replaces the flag package's default listing. That listing sorts
// alphabetically, which puts the destructive -del first and buries -l, and it
// has nowhere to put the two caveats a new user most needs to know: UDP does not
// traverse the tunnel, and the default listen address is world-reachable with no
// authentication.
func usage() {
	out := flag.CommandLine.Output()
	fmt.Fprint(out, `warp —— Cloudflare WARP 客户端（MASQUE over QUIC/HTTP-3，SOCKS5 前端）

用法：
  warp [选项]

代理：
  -l <host:port>   SOCKS5 监听地址（默认 :40000，同时接受 IPv4 与 IPv6 客户端）
  -user <用户名>   SOCKS5 用户名；必须同时给出 -user 和 -pass 才启用认证
  -pass <密码>     SOCKS5 密码
  -ip <取值>       连接哪个边缘（默认 4）：
                     4            注册信息中的 IPv4 边缘
                     6            注册信息中的 IPv6 边缘
                     <host:port>  改为连接指定地址，例如 162.159.198.2:4500、
                                  [2606:4700:103::2]:443、example.com:443
                   它决定的是"如何到达边缘"，不限制隧道内能访问什么 —— 目标
                   由边缘代为连接，所以走 IPv4 边缘一样能访问 IPv6-only 站点。
                   取 4 或 6 时会遍历注册给出的整个端口列表；给显式 host:port
                   时只使用该端口。域名由系统解析器解析（此时隧道尚未建立），
                   解析出的每个地址都会作为候选。

注册：
  -reg             尚未注册时执行注册，然后退出
  -del             向 API 注销并删除本地注册信息

注册信息保存在工作目录下的 reg.json。首次使用需先注册：启动本身从不注册，
因为创建账号是一个需要明确表达的动作。-reg 是幂等的 —— 已有注册时只报告并
退出，而不是替换掉它；替换会让旧注册在 Cloudflare 侧失去本地凭据，再也无法
注销。要更换注册，请先用 -del。

边缘地址与端口列表来自注册信息，因此没有单独的端点参数；端口按 API 返回的
顺序尝试，并从上次成功的那个开始。

示例：
  warp -reg                               注册（首次使用）
  warp                                    用已保存的注册信息运行
  warp -ip 6                              通过 IPv6 连接边缘
  warp -ip 162.159.198.2:4500             指定边缘地址与端口
  warp -ip example.com:443                通过域名连接自定义边缘
  warp -l 127.0.0.1:1080                  只监听回环地址
  warp -l 0.0.0.0:1080 -user u -pass s    对外提供服务并要求认证
  warp -del && warp -reg                  更换注册

扫描（可选，默认关闭）：
  扫描用与正式连接同一协议栈的轻量 QUIC 握手探针，对 WARP 边缘全段测真实往返
  延迟，按 RTT 升序取最低的 N 个端点前置到候选列表；注册端点作兜底尾接。不修改
  reg.json，失败回退到注册端点，不致命。-ip 4 扫 IPv4 段、-ip 6 扫 IPv6 段；
  -ip <host:port> 指定显式端点时扫描被忽略（显式端点优先）。详见用法末尾。

  warp -scan                              扫描并选用最低延迟的 IPv4 端点
  warp -scan -ip 6                        扫描 IPv6 段
  warp -scan -scan-ports 443              只扫 443 端口（更快覆盖更广）
  warp -scan -scan-top 8                  选用 8 个端点而非默认 4

参数：
  -scan                    启动前扫描 WARP 边缘全段并选用最低延迟的端点
  -scan-cidr <cidr,...>    追加自定义 CIDR 到默认段（默认 5 个 WARP IPv4 段或 2 个 IPv6 段）
  -scan-ports <p,p,...>    覆盖扫描端口（空则读 reg.json 的 endpoint_ports）
  -scan-concurrency <n>    并发探针数（0=自动 min(64, NumCPU*8)，下限 16）
  -scan-timeout <dur>      扫描总超时（默认 45s）
  -scan-per-probe <dur>    单探针超时（默认 3s）
  -scan-top <n>            选用 RTT 最低的 N 个端点前置（默认 4）

注意：
  UDP ASSOCIATE 的数据报不经过 WARP 隧道。plain CONNECT 是字节流隧道，无法
  承载数据报，因此它们从本机网络栈直接发出，对端看到的是你的真实地址。
  TCP 走隧道，UDP 不走。

  默认监听地址接受来自任何位置的连接，且不要求认证。在不可信网络中请绑定
  回环地址（-l 127.0.0.1:40000），或设置 -user 与 -pass。

`)
}

// edgeLookupTimeout bounds the bootstrap name lookup for an -ip hostname.
const edgeLookupTimeout = 10 * time.Second

// resolveEdge turns an explicit -ip value into the candidate address list.
//
// A hostname has to be resolved by the system resolver: this runs before the
// tunnel exists, so the in-tunnel DoH client is not available yet. That means a
// hostname here is visible to the local resolver — the same exposure the
// registration API call already has, but worth knowing about. An IP literal
// avoids it entirely.
//
// Every address the name resolves to becomes a candidate, so a dual-stack
// hostname still works on a single-stack host: the families this host cannot
// route are rejected immediately by the dialer.
func resolveEdge(spec string) ([]string, error) {
	host, port, err := net.SplitHostPort(spec)
	if err != nil {
		return nil, fmt.Errorf("需要 host:port 形式，例如 162.159.198.2:443、"+
			"[2606:4700:103::2]:443 或 example.com:443（%w）", err)
	}
	if host == "" {
		return nil, errors.New("需要 host:port 形式，主机部分为空")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return nil, fmt.Errorf("端口 %q 不是 1-65535 范围内的数字", port)
	}

	if net.ParseIP(host) != nil {
		return []string{net.JoinHostPort(host, port)}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), edgeLookupTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("用系统解析器解析 %q 失败：%w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("%q 未解析出任何地址", host)
	}

	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, net.JoinHostPort(a.IP.String(), port))
	}
	return out, nil
}

func main() {
	var (
		listen = flag.String("l", ":40000", "SOCKS5 监听地址 host:port")
		user   = flag.String("user", "", "SOCKS5 用户名（与 -pass 同时给出才启用认证）")
		pass   = flag.String("pass", "", "SOCKS5 密码（与 -user 同时给出才启用认证）")
		ip     = flag.String("ip", "4", "WARP 边缘：4、6，或显式 host:port")
		reg    = flag.Bool("reg", false, "尚未注册时执行注册，然后退出")
		del    = flag.Bool("del", false, "向 API 注销并删除本地注册信息")

		// 扫描（可选，默认关闭）：启动前对 WARP 边缘全段做真实 QUIC 握手探针，
		// 按 RTT 升序取 top-N 端点前置到 edgeAddrs，注册端点作兜底尾接。失败回退
		// 到注册端点，不致命（与方案 §5.4 一致：不写回 reg.json，保留 -reg/-del 幂等）。
		scan         = flag.Bool("scan", false, "启动前扫描 WARP 边缘全段并选用最低延迟的端点")
		scanCidr     = flag.String("scan-cidr", "", "逗号分隔，追加自定义 CIDR 到默认段（4 或 6）")
		scanPorts    = flag.String("scan-ports", "", "覆盖扫描端口（空则读 reg.json 的 endpoint_ports）")
		scanConc     = flag.Int("scan-concurrency", 0, "并发探针数（0 为自动 min(64, NumCPU*8)，下限 16）")
		scanTimeout  = flag.Duration("scan-timeout", 45*time.Second, "扫描总超时（硬上限）")
		scanPerProbe = flag.Duration("scan-per-probe", 3*time.Second, "单探针超时")
		scanTop      = flag.Int("scan-top", 4, "选用 RTT 最低的 N 个端点前置")
	)
	flag.Usage = usage
	flag.Parse()

	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)

	// -del and -reg are administrative: each does its one job and exits, so that
	// registering cannot be confused with starting the proxy.
	if *del {
		log.Println("正在注销...")
		if err := registration.DeleteRegistration(defaultStateFile); err != nil {
			log.Fatalf("注销失败：%v", err)
		}
		log.Println("✓ 注销成功")
		return
	}

	if *reg {
		// Registering is idempotent: an existing registration is left alone.
		// Replacing it silently would strand the old one on Cloudflare's side
		// with no local credential left to delete it.
		switch existing, err := registration.Load(defaultStateFile); {
		case err == nil:
			log.Printf("已注册：id=%s（%s）", existing.ID, defaultStateFile)
			log.Println("无需操作。要换一个注册，请先用 -del 注销。")
			return
		case !errors.Is(err, fs.ErrNotExist):
			log.Fatalf("%s 存在但无法读取（%v）。\n"+
				"拒绝覆盖：请删除该文件，或先执行 -del。", defaultStateFile, err)
		}

		regData, err := registration.Register()
		if err != nil {
			log.Fatalf("注册失败：%v", err)
		}
		if err := regData.Save(defaultStateFile); err != nil {
			log.Fatalf("注册信息写入 %s 失败：%v", defaultStateFile, err)
		}
		log.Printf("✓ 注册信息已保存到 %s（id=%s）", defaultStateFile, regData.ID)
		log.Println("不带 -reg 运行即可启动代理。")
		return
	}

	// Starting never registers: creating an account is an explicit act, and
	// doing it implicitly would leave a registration on Cloudflare's side that
	// the user never asked for.
	regData, err := registration.Load(defaultStateFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			log.Fatalf("%s 中没有注册信息。请先执行 warp -reg。", defaultStateFile)
		}
		log.Fatalf("无法读取注册文件 %s：%v", defaultStateFile, err)
	}
	log.Printf("✓ 已注册：id=%s", regData.ID)

	// -ip selects the edge: "4"/"6" pick the family from the registration —
	// which hands out both, though a host reaches only the families its network
	// carries — and anything else is an explicit address that replaces it.
	var edgeAddrs []string
	switch *ip {
	case "4", "6":
		endpointHost, other := regData.EndpointV4, "6"
		if *ip == "6" {
			endpointHost, other = regData.EndpointV6, "4"
		}
		if endpointHost == "" {
			log.Fatalf("注册信息中没有 IPv%s 边缘地址。"+
				"可改用 -ip %s，或依次执行 -del 与 -reg 重新注册。", *ip, other)
		}

		ports := regData.EndpointPorts
		if len(ports) == 0 {
			ports = []int{443}
		}
		for _, p := range ports {
			edgeAddrs = append(edgeAddrs, net.JoinHostPort(endpointHost, strconv.Itoa(p)))
		}
		log.Printf("WARP 代理启动中（边缘=IPv%s %s 端口=%v，socks5=%s）",
			*ip, endpointHost, ports, *listen)

	default:
		var err error
		if edgeAddrs, err = resolveEdge(*ip); err != nil {
			log.Fatalf("-ip %q 既不是 4 或 6，也不是可用地址：%v", *ip, err)
		}
		log.Printf("WARP 代理启动中（边缘=%s → %v，socks5=%s）", *ip, edgeAddrs, *listen)
	}

	// Pin the edge to the endpoint public key from registration, like warp-svc does.
	verifyEdge, err := regData.PeerPublicKeyVerifier()
	if err != nil {
		log.Fatalf("边缘公钥固定初始化失败：%v", err)
	}

	// Build TLS config for MASQUE connection.
	// The SNI is a well-known name rather than the edge's own identity and the
	// chain is signed by a private CA, so the standard chain check cannot apply;
	// authentication comes from pinning the endpoint public key instead.
	tlsConfig := &tls.Config{
		ServerName:            "consumer-masque-proxy.cloudflareclient.com",
		NextProtos:            []string{"h3"},
		InsecureSkipVerify:    true,
		MinVersion:            tls.VersionTLS13,
		Certificates:          []tls.Certificate{regData.ClientCert},
		VerifyPeerCertificate: verifyEdge,

		// warp-svc offers only the NIST curves — its ClientCertificateHook sets
		// P-256/P-384/P-521 and never X25519. Go would otherwise lead with
		// X25519, and the edge answers a key share it does not want with a
		// HelloRetryRequest, costing an extra round trip on every handshake.
		CurvePreferences: []tls.CurveID{tls.CurveP256, tls.CurveP384, tls.CurveP521},
	}
	if verifyEdge != nil {
		log.Println("✓ 边缘公钥固定已启用")
	} else {
		log.Println("⚠ 注册信息中没有边缘公钥，公钥固定已禁用（请重新执行 -reg）")
	}

	// 可选：启动前扫描 WARP 边缘全段，按 RTT 升序把 top-N 端点前置到 edgeAddrs。
	// 注册端点尾接作兜底，扫描全失败时 edgeAddrs 原样保留（回退注册端点，不致命）。
	// -ip 为显式 host:port 时扫描被忽略 —— 显式端点优先于自动优选。
	if *scan {
		switch *ip {
		case "4", "6":
			edgeAddrs = runEndpointScan(*ip == "6", edgeAddrs, regData, tlsConfig,
				*scanCidr, *scanPorts, *scanConc, *scanTimeout, *scanPerProbe, *scanTop)
		default:
			log.Printf("⚠ -ip %q 指定了显式端点，-scan 被忽略（显式端点优于自动优选）", *ip)
		}
	}

	// Connect to WARP edge via QUIC/H3
	proxyClient, err := tunnel.NewMasqueClient(edgeAddrs, tlsConfig, regData.Token)
	if err != nil {
		log.Fatalf("MASQUE 连接失败：%v", err)
	}
	defer proxyClient.Close()
	log.Println("✓ MASQUE 连接已建立")

	// Start SOCKS5 proxy server
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("SOCKS5 监听失败：%v", err)
	}
	defer ln.Close()

	socksCfg := tunnel.SOCKS5Config{
		Username: *user,
		Password: *pass,
		AllowUDP: true,
	}

	authInfo := ""
	if *user != "" && *pass != "" {
		authInfo = fmt.Sprintf("（认证用户：%s）", *user)
	}
	log.Printf("SOCKS5 代理监听于 %s%s", *listen, authInfo)
	log.Println("UDP ASSOCIATE 已启用 —— 数据报从本机直接发出，不经过 WARP 隧道")

	// Handle connections
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		<-sigCh
		log.Println("正在关闭...")
		cancel()   // signal all HandleSOCKS5 goroutines to stop
		ln.Close() // unblock Accept
	}()

	// Accept errors are not all fatal. Running out of file descriptors or having
	// a client vanish between the SYN and the accept is transient: backing off
	// and continuing keeps the proxy alive, where returning would take the whole
	// process down over a momentary condition.
	const maxAcceptBackoff = time.Second
	var acceptBackoff time.Duration

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break // graceful shutdown
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			if errors.Is(err, net.ErrClosed) {
				break // listener gone and not a shutdown we initiated
			}
			if acceptBackoff == 0 {
				acceptBackoff = 5 * time.Millisecond
			} else if acceptBackoff *= 2; acceptBackoff > maxAcceptBackoff {
				acceptBackoff = maxAcceptBackoff
			}
			log.Printf("Accept 出错：%v，%s 后重试", err, acceptBackoff)
			select {
			case <-time.After(acceptBackoff):
			case <-ctx.Done():
				return
			}
			continue
		}
		acceptBackoff = 0
		go proxyClient.HandleSOCKS5(ctx, conn, socksCfg)
	}
}

// runEndpointScan 在启动前对 WARP 边缘全段做扫描优选，返回替换后的 edgeAddrs。
//
// 行为契约（与方案 §5 对齐）：
//   - v6=true 扫描 IPv6 默认段，否则扫描 IPv4 默认段。额外段经 -scan-cidr 追加。
//   - 端口集合：-scan-ports 非空时覆盖，否则复用 regData.EndpointPorts（与正式
//     连接的端口回退候选集一致，语义自洽）。
//   - 成功：top-N 端点前置 + 注册端点尾接兜底，返回新 edgeAddrs。
//   - 失败：edgeAddrs 原样返回，打 warning，不致命 —— 上层照常用注册端点。
//   - 公钥固定：tlsConfig 由 scanner 透传给每个探针，探针内部在 QUIC 握手阶段会
//     调用 VerifyPeerCertificate，故扫描就在 WARP 同组边缘内进行（方案 §7.4）。
//
// 这是 main.go 的私有编排，不含扫描算法本身 —— 算法在 scanner.Scan。
func runEndpointScan(
	v6 bool,
	fallback []string, // 注册端点（尾接兜底）
	reg *registration.Registration,
	tlsConfig *tls.Config,
	scanCidr, scanPorts string,
	scanConc int,
	scanTimeout, scanPerProbe time.Duration,
	scanTop int,
) []string {
	// 默认段 + 用户追加段。
	cidrs := scanner.DefaultV4CIDRs()
	fam := "IPv4"
	if v6 {
		cidrs = scanner.DefaultV6CIDRs()
		fam = "IPv6"
	}
	if extra, ok := parseCIDRList(scanCidr); ok {
		cidrs = append(cidrs, extra...)
	}

	// 端口：-scan-ports 优先，否则复用注册端口。
	ports := reg.EndpointPorts
	if pv, ok := parsePortList(scanPorts); ok {
		ports = pv
	}
	if len(ports) == 0 {
		ports = []int{443}
	}

	// 并发数：钳到"自动 min(64, NumCPU*8)、下限 16"由 scanner.ClampConcurrency 统一负责，
	// main.go 不重复实现同一算法（"两层默认各管一段"）。scanConc<=0 触发自动。
	conc := scanner.ClampConcurrency(scanConc)

	log.Printf("扫描 WARP %s 边缘（段=%d 个，端口=%v，并发=%d，总超时=%s，单探针=%s，top=%d）...",
		fam, len(cidrs), ports, conc, scanTimeout, scanPerProbe, scanTop)

	results, err := scanner.Scan(context.Background(), scanner.Config{
		CIDRs:           cidrs,
		Ports:           ports,
		TLSConfig:       tlsConfig,
		QUICConfig:      scanner.ProbeQuicConfig(),
		Concurrency:     conc,
		PerProbeTimeout: scanPerProbe,
		TotalTimeout:    scanTimeout,
		TopN:            scanTop,
		PerIPLimit:      scanner.DefaultPerIPLimit,
	})
	if err != nil {
		log.Printf("⚠ 扫描未得到可用端点（%v），回退到注册端点 %v", err, fallback)
		return fallback
	}

	// top-N 前置，注册端点尾接兜底。
	out := make([]string, 0, len(results)+len(fallback))
	topAddrs := make([]string, 0, len(results))
	for _, r := range results {
		out = append(out, r.Addr)
		topAddrs = append(topAddrs, r.Addr)
	}
	out = append(out, fallback...)
	log.Printf("✓ 扫描完成，选用 %d 个最低延迟 %s 端点：%s", len(results), fam, topAddrs)
	if len(fallback) > 0 {
		log.Printf("  注册端点尾接兜底：%v", fallback)
	}
	return out
}

// parseCIDRList 把逗号分隔的 CIDR 字符串解析成切片。空串返回 (nil,false) 表示
// "用户没有指定"，与"用户指定了空列表"区分。非法条目静默丢弃：一个坏段不应让
// 整个扫描停摆（与 BuildCandidates 对非法 CIDR 的处理一致）。
func parseCIDRList(s string) ([]string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, _, err := net.ParseCIDR(p); err != nil {
			log.Printf("⚠ 忽略非法 CIDR 段 %q（%v）", p, err)
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// parsePortList 把逗号分隔的端口字符串解析成切片。空串返回 (nil,false)。
// 非法端口静默丢弃，全部非法则返回 (nil,false) 让上层回退到注册端口。
func parsePortList(s string) ([]int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			log.Printf("⚠ 忽略非法端口 %q", p)
			continue
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

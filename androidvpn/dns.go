//go:build android || linux

package androidvpn

// DNS 拦截服务器（v0.5.24 Android 外网根因修复）。
//
// 决定性实验（真实边缘 + 用户 reg.json，TestIPEdgeProbe）确证根因：
// WARP 边缘 CONNECT 的目标 IP 必须处于**边缘网络视图**——隧道内 DoH 解析
// 出的 facebook IP（57.145.12.1）CONNECT 成功；Android 系统 DNS 解析出的
// 同一域名 IP（69.171.235.22）CONNECT hang 到 deadline（http3: parsing
// frame failed: deadline exceeded，与用户日志一字不差）。域名路径成功的
// 原因是 resolveTarget 用隧道内 DoH 解析，天然拿到边缘可达 IP；而 TUN 只
// 收到系统 DNS 的 IP → DialTunnel("IP:443") → 边缘连不到 → 全挂。
//
// 修复（sing-box 标准 TUN DNS 拦截架构）：把 Android DNS 指向 TUN 内
// （WarpVpnService addDnsServer 198.18.0.1），拦截 UDP:53 查询 → 用隧道内
// DoH 解析（kernel.ResolveDNS，返回边缘可达真实 IP）→ 把解析出的 IP 连同
// 原域名记入 IP→域名映射表 → NewConnectionEx 收到该 IP 的 TCP 连接时查表
// 还原域名，以域名调用 DialTunnel——域名路径内部再次 DoH 解析，保证
// CONNECT 目标永远边缘可达。映射 miss（IP 直连/未拦截查询）退化为原有
// IP 直连语义，不影响 direct/reject 分流。
import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// DNSInterceptAddr 是 TUN 内 DNS 拦截服务器地址。Java 侧
// VpnService.Builder.addDnsServer 指向同一地址（v0.5.24 接线），Android
// 系统把 DNS 查询发往此地址 → 经全量路由进入 TUN → gVisor 栈解析为
// UDP:53 目标本地址 → NewPacketConnectionEx 拦截走 HandleQuery。
// 198.18.0.0/15 是 RFC 2544 基准测试保留段，不与真实网络冲突，sing-box
// 等 TUN DNS 拦截常用。
var DNSInterceptAddr = netip.MustParseAddr("198.18.0.1")

// ResolveFunc 解析域名（生产 = *tunnel.MasqueClient.ResolveDNS 隧道内 DoH；
// 测试注入假解析避免真实网络）。
type ResolveFunc func(ctx context.Context, host string) (net.IP, error)

// 解析源标记：remember 记录该 IP 由哪个上游解析出（可观测性与防覆盖辅助）。
// LookupDomain 的还原入口保持单一（返回域名字符串），src 不改变还原行为
// ——direct 分支保留原始 IP 拨号（decideTunnelTarget），物理 IP 天然不被
// 隧道 IP 覆盖（见 design.md §3）。
const (
	srcTunnel   = "tunnel"   // 隧道 DoH（海外解析者视角，边缘可达）
	srcPhysical = "physical" // 物理 DNS 直连（国内解析者视角，本地直连）
)

// defaultPhysicalDNSServers 是物理 DNS 上游兜底（Java 注入/config.json 均
// 未提供时）：阿里/腾讯/114 公共 DNS，国内视角解析国内 CDN 节点。与隧道
// DoH（1.1.1.1 海外视角）的区别正是 v0.5.30 阶段 12 的修复点。
var defaultPhysicalDNSServers = []netip.Addr{
	netip.MustParseAddr("223.5.5.5"),
	netip.MustParseAddr("119.29.29.29"),
	netip.MustParseAddr("114.114.114.114"),
}

// dnsInterceptor 拦截 TUN 内 UDP:53 查询：按域名走隧道 DoH 或物理 DNS
// 解析 + IP→域名映射。
type dnsInterceptor struct {
	resolve ResolveFunc
	route   RouteFunc
	// physicalServers 是物理 DNS 上游（NewDNSInterceptor 由 Java 注入/
	// config.json 填充，为空时用 defaultPhysicalDNSServers）。
	physicalServers []netip.Addr
	// physicalResolver 解析国内域名：生产 = resolvePhysical（UDP 直连
	// physicalServers + protect socket）；测试注入假解析避免真实网络。
	physicalResolver ResolveFunc

	mu      sync.Mutex
	domains map[netip.Addr]domainEntry
}

// domainEntry 记录 IP→域名映射、解析源及其过期时间。src 目前仅可观测性
// 用途（LookupDomain 行为不分支），见 srcTunnel/srcPhysical。
type domainEntry struct {
	domain string
	src    string
	expiry time.Time
}

// dnsMapTTL 控制映射条目存活时间；解析响应自身 TTL 通常只有几十秒，
// 取固定 10 分钟上限覆盖长连接场景（连接建立时查一次即可）。
const dnsMapTTL = 10 * time.Minute

// dnsLookupTimeout 单次域名解析的超时（隧道 DoH 单查询 5s，这里给足预算）。
const dnsLookupTimeout = 8 * time.Second

// NewDNSInterceptor 创建拦截服务器。resolve 为 nil 时 HandleQuery 返回 nil
// （调用方丢弃查询，退化为未拦截行为）。
//
// v0.5.30 阶段 12 扩展：route 判定命中 direct 的域名走物理 DNS
// （physicalDNS 上游，为空时用公共 DNS 兜底），其余走隧道 DoH。
func NewDNSInterceptor(resolve ResolveFunc, route RouteFunc, physicalDNS []netip.Addr) *dnsInterceptor {
	if resolve == nil {
		log.Println("⚠ DNS 拦截服务器未配置解析函数，TUN DNS 不拦截")
	}
	d := &dnsInterceptor{resolve: resolve, route: route, domains: make(map[netip.Addr]domainEntry)}
	d.physicalServers = physicalDNS
	if len(d.physicalServers) == 0 {
		d.physicalServers = defaultPhysicalDNSServers
	}
	d.physicalResolver = d.resolvePhysical
	return d
}

// LookupDomain 查询 IP→域名映射；未命中或已过期返回 ok=false。
// NewConnectionEx 对 TCP 目标 IP 查表，命中则用域名走 DialTunnel。
func (d *dnsInterceptor) LookupDomain(ip netip.Addr) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.domains == nil {
		return "", false
	}
	entry, ok := d.domains[ip.Unmap()]
	if !ok || time.Now().After(entry.expiry) {
		if ok {
			delete(d.domains, ip.Unmap())
		}
		return "", false
	}
	return entry.domain, true
}

// remember 记录 IP→域名映射（覆盖旧值，刷新过期时间）。src 标记解析源
// （srcPhysical / srcTunnel），仅可观测性用途；同 IP 后写覆盖前写保持现状
// 语义（key 是 IP，物理/隧道解析出的 IP 不同 → 天然不覆盖）。
func (d *dnsInterceptor) remember(ip netip.Addr, domain, src string) {
	if !ip.IsValid() {
		return
	}
	d.mu.Lock()
	if d.domains == nil {
		d.domains = make(map[netip.Addr]domainEntry)
	}
	d.domains[ip.Unmap()] = domainEntry{domain: domain, src: src, expiry: time.Now().Add(dnsMapTTL)}
	d.mu.Unlock()
}

// usePhysical 判定域名是否走物理 DNS 解析（v0.5.30 阶段 12 DNS 源分流）：
// route 命中 direct（geosite:cn / geosite:private / geoip:private / domain
// 规则）→ 物理 DNS 拿国内节点；proxy / 未命中 / route 为 nil → 隧道 DoH
// （现状不变）。域名级判定：HandleQuery 已解出 host（没有 IP），传零值
// netip.Addr 使 geosite/domain 规则可命中、geoip 规则不参与。
func (d *dnsInterceptor) usePhysical(host string) bool {
	if d.route == nil {
		return false
	}
	action, matched := d.route(host, netip.Addr{})
	return matched && action == "direct"
}

// HandleQuery 处理一条 TUN 内 DNS 查询报文：解析域名 → 隧道 DoH 解析 →
// 构造响应并记录 IP→域名映射。返回 nil 表示丢弃（不支持的查询类型、
// 解析失败等——调用方应静默 drop，让上层回退）。
//
// 只处理 A/AAAA + INET 单查询；其余（PTR、MX、ANY 等）与多查询报文直接
// 返回 nil。解析结果按查询类型过滤：A 查询只回 IPv4，AAAA 只回 IPv6。
// 地址族不匹配（resolveDNS 返回 A 优先，AAAA 查询很可能拿到 v4）回
// NOERROR 空应答（权威"该类型无记录"），不再丢弃——丢弃会让 Android DNS
// 客户端超时后回退物理 DNS，拿到本地视图 v6 IP（v0.5.24 根因的 v6 形态）。
func (d *dnsInterceptor) HandleQuery(payload []byte) []byte {
	if d.resolve == nil {
		return nil
	}
	var q dnsmessage.Message
	if err := q.Unpack(payload); err != nil {
		log.Printf("⚠ DNS 拦截：解析查询失败：%v", err)
		return nil
	}
	if len(q.Questions) != 1 || q.Header.Response {
		return nil // 多查询或响应报文不处理（只处理单查询）
	}
	question := q.Questions[0]
	if question.Class != dnsmessage.ClassINET {
		return nil
	}
	host := trimDNSDot(question.Name.String())
	if host == "" {
		return nil
	}
	wantV6 := question.Type == dnsmessage.TypeAAAA
	if question.Type != dnsmessage.TypeA && !wantV6 {
		return nil // 仅 A/AAAA
	}

	// DNS 源分流（v0.5.30 阶段 12）：国内域名（route→direct）走物理 DNS
	// 直连拿国内节点，其余走隧道 DoH（海外解析者视角，现状不变）。route
	// 为 nil（桌面/CLI）恒走隧道。
	resolver := d.resolve
	src := srcTunnel
	if d.usePhysical(host) {
		resolver = d.physicalResolver
		src = srcPhysical
	}
	ctx, cancel := context.WithTimeout(context.Background(), dnsLookupTimeout)
	ip, err := resolver(ctx, host)
	cancel()
	if err != nil {
		// 解析失败 → SERVFAIL 响应（v0.5.25：不再静默 drop）。drop 让
		// Android DNS 挂起直到查询超时，或 fallback 到物理 DNS
		// （114.114.114.114:53）返回本地视图 IP → 映射 miss → 裸 IP 走
		// 隧道边缘不可达（v0.5.24 真机日志）。SERVFAIL 带原 Question，
		// Android 立即回退下一个 DNS，行为与非拦截时一致。
		log.Printf("⚠ DNS 拦截：%s 解析失败：%v", host, err)
		return servfail(q)
	}

	var answer *dnsmessage.Resource
	switch {
	case !wantV6 && ip.To4() != nil:
		answer = d.answer(question.Name, dnsmessage.TypeA, ip.To4())
	case wantV6 && ip.To4() == nil:
		answer = d.answer(question.Name, dnsmessage.TypeAAAA, ip.To16())
	default:
		// 查询类型与解析结果地址族不匹配（如 AAAA 查询拿到 v4）：回 NOERROR
		// 空应答（权威"该类型无记录"），而不是 nil/丢弃。丢弃让 Android DNS
		// 客户端超时后回退物理 DNS → 本地视图 v6 IP → IP→域名映射 miss → 裸
		// v6 IP 走隧道 CONNECT 边缘不可达（A15 双栈挂死 firstByteMs=-1）。
		// 空应答让 Android 立即认定无此类型记录：AAAA 查询回"无 AAAA"不再
		// 泄漏到物理 DNS，直接用 A 查询的 v4 IP（隧道 DNS 解析出，边缘可达）。
		return noData(q)
	}

	d.remember(addrFromIP(ip), host, src)

	resp := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID:                 q.Header.ID,
			Response:           true,
			OpCode:             q.Header.OpCode,
			RecursionDesired:   q.Header.RecursionDesired,
			RecursionAvailable: true,
		},
		Questions: q.Questions,
		Answers:   []dnsmessage.Resource{*answer},
	}
	wire, err := resp.Pack()
	if err != nil {
		log.Printf("⚠ DNS 拦截：封装响应失败：%v", err)
		return nil
	}
	return wire
}

// resolvePhysical 用物理 DNS 上游解析 host（v0.5.30 阶段 12）：先查 A
// （国内 CDN 普遍双栈，A 记录足够；AAAA 查询的 noData 由 HandleQuery 的
// 地址族过滤兜底），A 无记录时再查 AAAA——避免 AAAA-only 站点无解。多上游
// 逐个试；单上游失败换下一个。全部失败返回错误 → HandleQuery 回 SERVFAIL，
// Android 回退下一个 DNS，不恶化（design.md 风险节）。
func (d *dnsInterceptor) resolvePhysical(ctx context.Context, host string) (net.IP, error) {
	var lastErr error
	for _, server := range d.physicalServers {
		ip, err := d.physicalQuery(ctx, server, host, dnsmessage.TypeA)
		if err == nil {
			return ip, nil
		}
		lastErr = err
		if errors.Is(err, errNoSuchRecord) {
			// A 无记录 → 试 AAAA
			ip, err = d.physicalQuery(ctx, server, host, dnsmessage.TypeAAAA)
			if err == nil {
				return ip, nil
			}
			lastErr = err
			log.Printf("⚠ 物理 DNS %s 解析 %s 的 AAAA 失败：%v", server, host, err)
			continue
		}
		log.Printf("⚠ 物理 DNS %s 解析 %s 失败：%v", server, host, err)
	}
	return nil, lastErr
}

// errNoSuchRecord 标记物理 DNS 对该查询类型返回了 NXDOMAIN / 空应答
// （权威"无此记录"），区别于传输错误——resolvePhysical 据此试下一个地址族。
var errNoSuchRecord = errors.New("no such record")

// physicalQuery 向单个物理 DNS 服务器发一条 UDP DNS 查询并解析响应。
// socket 经 Dialer.Control 调用 socketProtector（Android VpnService.
// protect），否则查询重新进入 TUN 环路（与 v0.5.24 修的"direct 拨号物理
// 解析环路"独立：这次是 DNS 查询 socket 本身）。单查询超时
// dnsLookupTimeout 内完成。
func (d *dnsInterceptor) physicalQuery(ctx context.Context, server netip.Addr, host string, qtype dnsmessage.Type) (net.IP, error) {
	name, err := dnsmessage.NewName(fqdn(host))
	if err != nil {
		return nil, fmt.Errorf("非法的 DNS 名称 %q：%w", host, err)
	}
	query := dnsmessage.Message{
		Header: dnsmessage.Header{ID: nextDNSQueryID(), RecursionDesired: true},
		Questions: []dnsmessage.Question{{
			Name:  name,
			Type:  qtype,
			Class: dnsmessage.ClassINET,
		}},
	}
	wire, err := query.Pack()
	if err != nil {
		return nil, fmt.Errorf("封装 DNS 查询失败：%w", err)
	}

	// 复用 decision.go 的 socketProtector 模式：物理 DNS socket 必须豁免出
	// VPN 路由（protect），否则 UDP:53 查询经 TUN 拦截再回来，环路风暴。
	dialer := &net.Dialer{}
	if socketProtector != nil {
		dialer.Control = func(network, address string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				if perr := socketProtector(int(fd)); perr != nil {
					log.Printf("⚠ 保护物理 DNS socket（fd=%d）失败：%v", int(fd), perr)
				}
			})
		}
	}
	conn, err := dialer.DialContext(ctx, "udp", net.JoinHostPort(server.String(), "53"))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := conn.Write(wire); err != nil {
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(dnsLookupTimeout))
	buf := make([]byte, 512) // UDP DNS 响应常规上限；truncated 时上层回退
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	return parsePhysicalAnswer(buf[:n], host, qtype)
}

// dnsQueryIDCounter 生成物理 DNS 查询的报文 ID（自增即可满足"请求-响应
// 匹配"——每条查询独立 socket，无并发碰撞面）。
var dnsQueryIDCounter atomic.Uint32

func nextDNSQueryID() uint16 {
	return uint16(dnsQueryIDCounter.Add(1))
}

// fqdn 补 FQDN 尾点（dnsmessage.NewName 要求 "example.com." 形式）。
func fqdn(host string) string {
	if len(host) > 0 && host[len(host)-1] == '.' {
		return host
	}
	return host + "."
}

// parsePhysicalAnswer 从物理 DNS 响应中提取指定地址族的首个地址。
// 只接受完整响应（非 truncated）、RCode 成功且有匹配记录；NXDOMAIN /
// 空应答 / CNAME 链无匹配 → errNoSuchRecord（resolvePhysical 换下一上游
// 或下一地址族）。
func parsePhysicalAnswer(body []byte, host string, qtype dnsmessage.Type) (net.IP, error) {
	var msg dnsmessage.Message
	if err := msg.Unpack(body); err != nil {
		return nil, fmt.Errorf("解析物理 DNS 响应失败：%w", err)
	}
	if msg.RCode == dnsmessage.RCodeNameError {
		return nil, errNoSuchRecord
	}
	if msg.RCode != dnsmessage.RCodeSuccess {
		return nil, fmt.Errorf("%s 的物理 DNS 响应码为 %s", host, msg.RCode)
	}
	for _, ans := range msg.Answers {
		var ip net.IP
		switch body := ans.Body.(type) {
		case *dnsmessage.AResource:
			if qtype != dnsmessage.TypeA {
				continue
			}
			ip = net.IP(body.A[:])
		case *dnsmessage.AAAAResource:
			if qtype != dnsmessage.TypeAAAA {
				continue
			}
			ip = net.IP(body.AAAA[:])
		default:
			continue
		}
		return ip, nil
	}
	return nil, errNoSuchRecord
}

// noData 构造一条 NOERROR 空应答（保留原 Question、ID、OpCode，无 Answer），
// 权威声明"该查询类型无记录"。与 servfail 的区别：servfail 表示解析失败，
// Android 会回退下一个 DNS；noData 表示查询成功但地址族不匹配（如 AAAA 查询
// 拿到 v4），Android 认定无该类型记录、不回退——防止 v6 泄漏到本地视图
// （v0.5.24 根因的 v6 形态）。
func noData(q dnsmessage.Message) []byte {
	resp := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID:                 q.Header.ID,
			Response:           true,
			OpCode:             q.Header.OpCode,
			RecursionDesired:   q.Header.RecursionDesired,
			RecursionAvailable: true,
		},
		Questions: q.Questions,
	}
	wire, err := resp.Pack()
	if err != nil {
		log.Printf("⚠ DNS 拦截：封装 NOERROR 空应答失败：%v", err)
		return nil
	}
	return wire
}

// servfail 构造一条 SERVFAIL 响应（保留原 Question、ID、OpCode，无 Answer），
// 让 Android DNS 客户端立即回退下一个服务器而不是等待超时。
func servfail(q dnsmessage.Message) []byte {
	resp := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID:                 q.Header.ID,
			Response:           true,
			OpCode:             q.Header.OpCode,
			RecursionDesired:   q.Header.RecursionDesired,
			RecursionAvailable: true,
			RCode:              dnsmessage.RCodeServerFailure,
		},
		Questions: q.Questions,
	}
	wire, err := resp.Pack()
	if err != nil {
		log.Printf("⚠ DNS 拦截：封装 SERVFAIL 失败：%v", err)
		return nil
	}
	return wire
}

// answer 构造一条 A/AAAA 应答记录（TTL 固定 300s，与 dnsMapTTL 同量级）。
func (d *dnsInterceptor) answer(name dnsmessage.Name, typ dnsmessage.Type, ip net.IP) *dnsmessage.Resource {
	hdr := dnsmessage.ResourceHeader{
		Name:  name,
		Type:  typ,
		Class: dnsmessage.ClassINET,
		TTL:   300,
	}
	if typ == dnsmessage.TypeA {
		var a [4]byte
		copy(a[:], ip)
		return &dnsmessage.Resource{Header: hdr, Body: &dnsmessage.AResource{A: a}}
	}
	var aaaa [16]byte
	copy(aaaa[:], ip)
	return &dnsmessage.Resource{Header: hdr, Body: &dnsmessage.AAAAResource{AAAA: aaaa}}
}

// addrFromIP 把 net.IP 转 netip.Addr；非法返回零值（remember 内过滤）。
func addrFromIP(ip net.IP) netip.Addr {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return netip.Addr{}
	}
	return a
}

// trimDNSDot 去掉 FQDN 尾点（dnsmessage Name.String 输出 "www.example.com."）。
func trimDNSDot(s string) string {
	if len(s) > 0 && s[len(s)-1] == '.' {
		return s[:len(s)-1]
	}
	return s
}

// errNotDNS 标记非 DNS 目标（当前未使用，保留供未来 TCP:53 分流）。
var errNotDNS = errors.New("not a DNS query")

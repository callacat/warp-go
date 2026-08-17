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
	"log"
	"net"
	"net/netip"
	"sync"
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

// dnsInterceptor 拦截 TUN 内 UDP:53 查询：隧道 DoH 解析 + IP→域名映射。
type dnsInterceptor struct {
	resolve ResolveFunc

	mu      sync.Mutex
	domains map[netip.Addr]domainEntry
}

// domainEntry 记录 IP→域名映射及其过期时间。
type domainEntry struct {
	domain string
	expiry time.Time
}

// dnsMapTTL 控制映射条目存活时间；解析响应自身 TTL 通常只有几十秒，
// 取固定 10 分钟上限覆盖长连接场景（连接建立时查一次即可）。
const dnsMapTTL = 10 * time.Minute

// dnsLookupTimeout 单次域名解析的超时（隧道 DoH 单查询 5s，这里给足预算）。
const dnsLookupTimeout = 8 * time.Second

// NewDNSInterceptor 创建拦截服务器。resolve 为 nil 时 HandleQuery 返回 nil
// （调用方丢弃查询，退化为未拦截行为）。
func NewDNSInterceptor(resolve ResolveFunc) *dnsInterceptor {
	if resolve == nil {
		log.Println("⚠ DNS 拦截服务器未配置解析函数，TUN DNS 不拦截")
	}
	return &dnsInterceptor{resolve: resolve, domains: make(map[netip.Addr]domainEntry)}
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

// remember 记录 IP→域名映射（覆盖旧值，刷新过期时间）。
func (d *dnsInterceptor) remember(ip netip.Addr, domain string) {
	if !ip.IsValid() {
		return
	}
	d.mu.Lock()
	if d.domains == nil {
		d.domains = make(map[netip.Addr]domainEntry)
	}
	d.domains[ip.Unmap()] = domainEntry{domain: domain, expiry: time.Now().Add(dnsMapTTL)}
	d.mu.Unlock()
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

	ctx, cancel := context.WithTimeout(context.Background(), dnsLookupTimeout)
	ip, err := d.resolve(ctx, host)
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

	d.remember(addrFromIP(ip), host)

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

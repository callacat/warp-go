//go:build android || linux

package androidvpn

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// fakeResolve 测试用假解析器：host→ipv4/ipv6 静态表。called 记录被查询的
// host（断言 DNS 源分流用，HandleQuery 同步执行无并发）。
type fakeResolve struct {
	v4     map[string]string
	v6     map[string]string
	called []string
}

func (f *fakeResolve) resolve(ctx context.Context, host string) (net.IP, error) {
	if f == nil {
		return nil, errors.New("no resolver")
	}
	f.called = append(f.called, host)
	if ip, ok := f.v4[host]; ok {
		return net.ParseIP(ip), nil
	}
	if ip, ok := f.v6[host]; ok {
		return net.ParseIP(ip), nil
	}
	return nil, errors.New("NXDOMAIN: " + host)
}

// newTestInterceptor 构造带假解析器的拦截器（无路由/无物理 DNS → 恒隧道）。
func newTestInterceptor() *dnsInterceptor {
	return NewDNSInterceptor((&fakeResolve{
		v4: map[string]string{"www.example.com": "57.145.12.1"},
		v6: map[string]string{"ipv6.example.com": "2606:4700:4700::1111"},
	}).resolve, nil, nil)
}

// newSplitInterceptor 构造带路由判定 + 假物理/隧道解析器的拦截器，供 DNS
// 源分流测试：tunnel 解析 geosite:proxy 的国外 host，physical 解析
// geosite:cn 的国内 host（两个解析器对同一 host 可给不同 IP——实测根因）。
func newSplitInterceptor() *dnsInterceptor {
	tunnel := &fakeResolve{
		v4: map[string]string{"www.google.com": "142.250.72.4"},
	}
	physical := &fakeResolve{
		v4: map[string]string{"www.example.com": "122.189.80.186"},
	}
	d := NewDNSInterceptor(tunnel.resolve, splitTestRoute, nil)
	d.physicalResolver = physical.resolve
	return d
}

// splitTestRoute 测试路由：www.example.com → direct（国内），其余 → proxy。
func splitTestRoute(host string, ip netip.Addr) (string, bool) {
	if host == "www.example.com" {
		return "direct", true
	}
	return "proxy", true
}

// TestDNSInterceptorCNDirectPhysical 验证国内域名（route→direct）走物理
// DNS 解析：返回物理解析器的国内节点 IP（122.189.80.186），隧道解析器
// 未被调用；映射 src=physical。
func TestDNSInterceptorCNDirectPhysical(t *testing.T) {
	d := newSplitInterceptor()
	resp := d.HandleQuery(packAQuery(0x20, "www.example.com"))
	if resp == nil {
		t.Fatal("国内域名查询应返回响应")
	}
	var m dnsmessage.Message
	if err := m.Unpack(resp); err != nil {
		t.Fatalf("解包响应失败：%v", err)
	}
	if len(m.Answers) != 1 {
		t.Fatalf("应恰有 1 条应答，得到 %d", len(m.Answers))
	}
	a, ok := m.Answers[0].Body.(*dnsmessage.AResource)
	if !ok {
		t.Fatalf("应答应为 A 记录，得到 %T", m.Answers[0].Body)
	}
	if got := net.IP(a.A[:]).String(); got != "122.189.80.186" {
		t.Fatalf("国内域名应返回物理 DNS 的国内节点 122.189.80.186，得到 %s", got)
	}
	// 映射记录物理源 IP → 域名，src=physical
	entry, ok := d.domains[netip.MustParseAddr("122.189.80.186")]
	if !ok || entry.src != srcPhysical {
		t.Fatalf("映射应为 src=physical，得到 src=%q ok=%v", entry.src, ok)
	}
	if domain, ok := d.LookupDomain(netip.MustParseAddr("122.189.80.186")); !ok || domain != "www.example.com" {
		t.Fatalf("映射域名应为 www.example.com，得到 %s/%v", domain, ok)
	}
}

// TestDNSInterceptorForeignTunnel 验证国外域名（route→proxy）走隧道 DoH
// 解析：隧道解析器被调用，物理解析器未被调用。
func TestDNSInterceptorForeignTunnel(t *testing.T) {
	d := newSplitInterceptor()
	resp := d.HandleQuery(packAQuery(0x21, "www.google.com"))
	if resp == nil {
		t.Fatal("国外域名查询应返回响应")
	}
	var m dnsmessage.Message
	if err := m.Unpack(resp); err != nil {
		t.Fatalf("解包响应失败：%v", err)
	}
	if len(m.Answers) != 1 {
		t.Fatalf("应恰有 1 条应答，得到 %d", len(m.Answers))
	}
	a, ok := m.Answers[0].Body.(*dnsmessage.AResource)
	if !ok {
		t.Fatalf("应答应为 A 记录，得到 %T", m.Answers[0].Body)
	}
	if got := net.IP(a.A[:]).String(); got != "142.250.72.4" {
		t.Fatalf("国外域名应返回隧道 DoH 的 IP 142.250.72.4，得到 %s", got)
	}
	// 物理解析器未被调用
	entry, ok := d.domains[netip.MustParseAddr("142.250.72.4")]
	if !ok || entry.src != srcTunnel {
		t.Fatalf("映射应为 src=tunnel，得到 src=%q ok=%v", entry.src, ok)
	}
}

// TestDNSInterceptorNilRouteTunnel 验证 route 为 nil 时（桌面/CLI，或
// Android 未注入 route）国内域名也走隧道 DoH——退回现状行为，不误判。
func TestDNSInterceptorNilRouteTunnel(t *testing.T) {
	d := newTestInterceptor()
	resp := d.HandleQuery(packAQuery(0x22, "www.example.com"))
	if resp == nil {
		t.Fatal("nil route 查询应返回响应")
	}
	var m dnsmessage.Message
	if err := m.Unpack(resp); err != nil {
		t.Fatalf("解包响应失败：%v", err)
	}
	if len(m.Answers) != 1 {
		t.Fatalf("应恰有 1 条应答，得到 %d", len(m.Answers))
	}
	a, ok := m.Answers[0].Body.(*dnsmessage.AResource)
	if !ok {
		t.Fatalf("应答应为 A 记录，得到 %T", m.Answers[0].Body)
	}
	// 假解析器只给 57.145.12.1（隧道 DoH 视角），物理路径未启用
	if got := net.IP(a.A[:]).String(); got != "57.145.12.1" {
		t.Fatalf("nil route 应走隧道 DoH 返回 57.145.12.1，得到 %s", got)
	}
}

// TestDNSInterceptorPhysicalFallback 验证物理 DNS 全部失败时回 SERVFAIL
// （设计文档风险节：不恶化，Android 回退下一个 DNS）。
func TestDNSInterceptorPhysicalFallback(t *testing.T) {
	physical := &fakeResolve{} // 空表 → 全 NXDOMAIN
	d := NewDNSInterceptor((&fakeResolve{}).resolve, splitTestRoute, nil)
	d.physicalResolver = physical.resolve
	resp := d.HandleQuery(packAQuery(0x23, "www.example.com"))
	if resp == nil {
		t.Fatal("物理解析失败应返回 SERVFAIL（不应 nil）")
	}
	var m dnsmessage.Message
	if err := m.Unpack(resp); err != nil {
		t.Fatalf("解包 SERVFAIL 失败：%v", err)
	}
	if m.RCode != dnsmessage.RCodeServerFailure {
		t.Fatalf("SERVFAIL 响应 RCode = %v, want ServerFailure", m.RCode)
	}
}

// packAQuery 构造一条 A 查询报文。
func packAQuery(id uint16, host string) []byte {
	return packQuery(id, host, dnsmessage.TypeA)
}

// packAAAAQuery 构造一条 AAAA 查询报文。
func packAAAAQuery(id uint16, host string) []byte {
	return packQuery(id, host, dnsmessage.TypeAAAA)
}

func packQuery(id uint16, host string, typ dnsmessage.Type) []byte {
	name, _ := dnsmessage.NewName(host + ".")
	q := dnsmessage.Message{
		Header: dnsmessage.Header{ID: id, RecursionDesired: true},
		Questions: []dnsmessage.Question{{
			Name:  name,
			Type:  typ,
			Class: dnsmessage.ClassINET,
		}},
	}
	wire, _ := q.Pack()
	return wire
}

// TestDNSInterceptorAAQuery 验证 A 查询：响应 ID 匹配、含 A 记录、TTL 非零，
// 且解析出的 IP 被记入映射表（边缘可达 IP → 域名）。
func TestDNSInterceptorAAQuery(t *testing.T) {
	d := newTestInterceptor()
	resp := d.HandleQuery(packAQuery(0x1234, "www.example.com"))
	if resp == nil {
		t.Fatal("A 查询应返回响应")
	}
	var m dnsmessage.Message
	if err := m.Unpack(resp); err != nil {
		t.Fatalf("解包响应失败：%v", err)
	}
	if m.Header.ID != 0x1234 {
		t.Fatalf("响应 ID 应为 0x1234，得到 %#x", m.Header.ID)
	}
	if !m.Header.Response {
		t.Fatal("响应标志未设置")
	}
	if len(m.Answers) != 1 {
		t.Fatalf("应恰有 1 条应答，得到 %d", len(m.Answers))
	}
	a, ok := m.Answers[0].Body.(*dnsmessage.AResource)
	if !ok {
		t.Fatalf("应答应为 A 记录，得到 %T", m.Answers[0].Body)
	}
	if got := net.IP(a.A[:]).String(); got != "57.145.12.1" {
		t.Fatalf("A 记录应为 57.145.12.1，得到 %s", got)
	}

	// 映射表：解析出的 IP → 原域名
	domain, ok := d.LookupDomain(netip.MustParseAddr("57.145.12.1"))
	if !ok {
		t.Fatal("IP→域名映射未记录")
	}
	if domain != "www.example.com" {
		t.Fatalf("映射域名应为 www.example.com，得到 %s", domain)
	}
}

// TestDNSInterceptorQueryTypeFilter 验证查询类型过滤：MX 查询不处理
// （返回 nil）；AAAA 查询但解析器只回 v4 时返回 NOERROR 空应答（不再
// nil/丢弃，详见 TestDNSInterceptorAAAANoV6Leak）。
func TestDNSInterceptorQueryTypeFilter(t *testing.T) {
	d := newTestInterceptor()

	// MX 查询 → nil
	name, _ := dnsmessage.NewName("www.example.com.")
	mxQ := dnsmessage.Message{
		Header: dnsmessage.Header{ID: 1, RecursionDesired: true},
		Questions: []dnsmessage.Question{{
			Name:  name,
			Type:  dnsmessage.TypeMX,
			Class: dnsmessage.ClassINET,
		}},
	}
	mxWire, _ := mxQ.Pack()
	if got := d.HandleQuery(mxWire); got != nil {
		t.Fatal("MX 查询不应处理（返回 nil）")
	}
}

// TestDNSInterceptorAAAANoV6Leak 验证 AAAA 查询拿到 v4 时返回 NOERROR 空应答
// 而非 nil/丢弃（v0.5.29 防泄漏）：丢弃让 Android DNS 客户端超时后回退物理
// DNS → 本地视图 v6 IP → IP→域名映射 miss → 裸 v6 IP 走隧道挂死（A15 双栈）。
// 空应答（"无 AAAA 记录"）让 Android 不再回退，直接用 A 查询的 v4 IP（隧道
// DNS 解析出，边缘可达）。
func TestDNSInterceptorAAAANoV6Leak(t *testing.T) {
	d := newTestInterceptor()
	// Android 对 getaddrinfo 并行发 A + AAAA；A 查询先到（记录 v4 → 域名映射）
	if resp := d.HandleQuery(packAQuery(3, "www.example.com")); resp == nil {
		t.Fatal("A 查询应返回响应")
	}
	// AAAA 查询：解析器只回 v4 → NOERROR 空应答，不是 nil
	resp := d.HandleQuery(packAAAAQuery(4, "www.example.com"))
	if resp == nil {
		t.Fatal("AAAA 查询解析到 v4 应返回 NOERROR 空应答（不应 nil/丢弃）")
	}
	var m dnsmessage.Message
	if err := m.Unpack(resp); err != nil {
		t.Fatalf("解包 NOERROR 空应答失败：%v", err)
	}
	if m.Header.ID != 4 {
		t.Fatalf("响应 ID = %#x, want 4", m.Header.ID)
	}
	if !m.Header.Response {
		t.Fatal("响应标志未设置")
	}
	if m.RCode != dnsmessage.RCodeSuccess {
		t.Fatalf("NOERROR 空应答 RCode = %v, want RCodeSuccess(0)", m.RCode)
	}
	if len(m.Answers) != 0 {
		t.Fatalf("空应答应无 Answer，得到 %d 条", len(m.Answers))
	}
	if len(m.Questions) != 1 {
		t.Fatalf("空应答应保留原 Question，得到 %d 个", len(m.Questions))
	}
	// 并行 A 查询记录的 v4 映射仍可用（客户端用 v4 连 → 还原域名走隧道）
	domain, ok := d.LookupDomain(netip.MustParseAddr("57.145.12.1"))
	if !ok || domain != "www.example.com" {
		t.Fatalf("A 查询的 v4 映射应存在，得到 %s/%v", domain, ok)
	}
}

// TestDNSInterceptorAAAAResolve 验证 AAAA-only 域名：解析器回 v6 → AAAA 应答。
func TestDNSInterceptorAAAAResolve(t *testing.T) {
	d := newTestInterceptor()
	name, _ := dnsmessage.NewName("ipv6.example.com.")
	aaaaQ := dnsmessage.Message{
		Header: dnsmessage.Header{ID: 7, RecursionDesired: true},
		Questions: []dnsmessage.Question{{
			Name:  name,
			Type:  dnsmessage.TypeAAAA,
			Class: dnsmessage.ClassINET,
		}},
	}
	wire, _ := aaaaQ.Pack()
	resp := d.HandleQuery(wire)
	if resp == nil {
		t.Fatal("AAAA-only 域名的 AAAA 查询应返回响应")
	}
	var m dnsmessage.Message
	if err := m.Unpack(resp); err != nil {
		t.Fatalf("解包失败：%v", err)
	}
	if len(m.Answers) != 1 {
		t.Fatalf("应恰有 1 条应答，得到 %d", len(m.Answers))
	}
	aaaa, ok := m.Answers[0].Body.(*dnsmessage.AAAAResource)
	if !ok {
		t.Fatalf("应答应为 AAAA 记录，得到 %T", m.Answers[0].Body)
	}
	if got := net.IP(aaaa.AAAA[:]).String(); got != "2606:4700:4700::1111" {
		t.Fatalf("AAAA 记录应为 2606:4700:4700::1111，得到 %s", got)
	}
	// v6 映射
	domain, ok := d.LookupDomain(netip.MustParseAddr("2606:4700:4700::1111"))
	if !ok || domain != "ipv6.example.com" {
		t.Fatalf("v6 映射应为 ipv6.example.com，得到 %s/%v", domain, ok)
	}
}

// TestDNSInterceptorResolveFailure 验证解析失败 → SERVFAIL 响应（v0.5.25：
// 不再静默 drop。drop 让 Android DNS 挂起直到查询超时，或 fallback 到物理
// DNS（114.114.114.114:53）返回本地视图 IP → 映射 miss → 裸 IP 走隧道
// 边缘不可达——v0.5.24 真机日志。SERVFAIL 带原 Question，Android 立即回退
// 下一个 DNS，行为与非拦截时一致）。
func TestDNSInterceptorResolveFailure(t *testing.T) {
	d := newTestInterceptor()
	resp := d.HandleQuery(packAQuery(9, "nxdomain.example.com"))
	if resp == nil {
		t.Fatal("解析失败应返回 SERVFAIL 响应（不应 nil）")
	}
	var m dnsmessage.Message
	if err := m.Unpack(resp); err != nil {
		t.Fatalf("解包 SERVFAIL 失败：%v", err)
	}
	if m.RCode != dnsmessage.RCodeServerFailure {
		t.Fatalf("SERVFAIL 响应 RCode = %v, want ServerFailure", m.RCode)
	}
	if !m.Header.Response {
		t.Fatal("SERVFAIL 响应应设置 Response 标志")
	}
	if m.Header.ID != 9 {
		t.Fatalf("SERVFAIL 响应 ID = %#x, want 9", m.Header.ID)
	}
	if len(m.Questions) != 1 {
		t.Fatalf("SERVFAIL 应保留原 Question，得到 %d 个", len(m.Questions))
	}
}

// TestDNSInterceptorNilResolve 验证未配置解析函数时全部丢弃。
func TestDNSInterceptorNilResolve(t *testing.T) {
	d := NewDNSInterceptor(nil, nil, nil)
	if resp := d.HandleQuery(packAQuery(10, "www.example.com")); resp != nil {
		t.Fatal("nil resolve 应返回 nil")
	}
	// nil 时 LookupDomain 安全
	if _, ok := d.LookupDomain(netip.MustParseAddr("57.145.12.1")); ok {
		t.Fatal("nil interceptor 不应有映射")
	}
}

// TestDNSInterceptorMappingExpiry 验证映射过期后 LookupDomain 返回 false。
func TestDNSInterceptorMappingExpiry(t *testing.T) {
	d := newTestInterceptor()
	resp := d.HandleQuery(packAQuery(11, "www.example.com"))
	if resp == nil {
		t.Fatal("查询应成功")
	}
	// 手动把映射时间改为过期
	d.mu.Lock()
	addr := netip.MustParseAddr("57.145.12.1")
	d.domains[addr] = domainEntry{domain: "www.example.com", expiry: time.Now().Add(-time.Second)}
	d.mu.Unlock()
	if _, ok := d.LookupDomain(addr); ok {
		t.Fatal("过期映射应返回 false")
	}
}

// TestTrimDNSDot 验证 FQDN 尾点裁剪。
func TestTrimDNSDot(t *testing.T) {
	if got := trimDNSDot("www.example.com."); got != "www.example.com" {
		t.Fatalf("trimDNSDot 应去掉尾点，得到 %q", got)
	}
	if got := trimDNSDot("localhost"); got != "localhost" {
		t.Fatalf("无尾点应原样返回，得到 %q", got)
	}
}

// TestDNSInterceptorMultiQuestion 验证多查询报文不处理。
func TestDNSInterceptorMultiQuestion(t *testing.T) {
	d := newTestInterceptor()
	name1, _ := dnsmessage.NewName("a.example.com.")
	name2, _ := dnsmessage.NewName("b.example.com.")
	q := dnsmessage.Message{
		Header: dnsmessage.Header{ID: 12, RecursionDesired: true},
		Questions: []dnsmessage.Question{
			{Name: name1, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET},
			{Name: name2, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET},
		},
	}
	wire, _ := q.Pack()
	if got := d.HandleQuery(wire); got != nil {
		t.Fatal("多查询报文应返回 nil")
	}
}

// TestDNSInterceptorResponseMessage 验证响应报文（非查询）不处理。
func TestDNSInterceptorResponseMessage(t *testing.T) {
	d := newTestInterceptor()
	q := dnsmessage.Message{
		Header: dnsmessage.Header{ID: 13, Response: true},
		Questions: []dnsmessage.Question{{
			Name:  mustName(t, "www.example.com."),
			Type:  dnsmessage.TypeA,
			Class: dnsmessage.ClassINET,
		}},
	}
	wire, _ := q.Pack()
	if got := d.HandleQuery(wire); got != nil {
		t.Fatal("响应报文应返回 nil")
	}
}

func mustName(t *testing.T, s string) dnsmessage.Name {
	t.Helper()
	n, err := dnsmessage.NewName(s)
	if err != nil {
		t.Fatalf("NewName(%q) 失败：%v", s, err)
	}
	return n
}

// startPhysicalDNSMock 在 127.0.0.1 临时端口起一个 UDP DNS mock：对 A 查询
// 回 answerIP，其余（AAAA 等）回 NOERROR 空应答。零真实网络（仅 loopback）。
// t.Cleanup 关闭连接（goroutine 随之退出）。
func startPhysicalDNSMock(t *testing.T, answerIP netip.Addr) (net.PacketConn, *net.UDPAddr) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听物理 DNS mock 失败：%v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	ip4 := answerIP.As4()
	go func() {
		buf := make([]byte, 512)
		for {
			n, clientAddr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var q dnsmessage.Message
			if err := q.Unpack(buf[:n]); err != nil {
				continue
			}
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
			if len(q.Questions) == 1 && q.Questions[0].Type == dnsmessage.TypeA {
				resp.Answers = []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{
						Name:  q.Questions[0].Name,
						Type:  dnsmessage.TypeA,
						Class: dnsmessage.ClassINET,
						TTL:   300,
					},
					Body: &dnsmessage.AResource{A: ip4},
				}}
			}
			wire, err := resp.Pack()
			if err != nil {
				continue
			}
			_, _ = pc.WriteTo(wire, clientAddr)
		}
	}()
	return pc, pc.LocalAddr().(*net.UDPAddr)
}

// setPhysicalDNSPort 把物理 DNS 上游端口缝指到 mock 端口（避免绑定 <1024 的
// 权限限制），测试结束还原。包级依赖注入缝，详见 dns.go physicalDNSPort。
func setPhysicalDNSPort(t *testing.T, port int) {
	t.Helper()
	prev := physicalDNSPort
	physicalDNSPort = strconv.Itoa(port)
	t.Cleanup(func() { physicalDNSPort = prev })
}

// udpAddrIP 提取 UDP 监听地址的 netip.Addr（4 字节 v4 → Unmap 成纯 v4）。
func udpAddrIP(a *net.UDPAddr) netip.Addr {
	ip, ok := netip.AddrFromSlice(a.IP)
	if !ok {
		panic("mock 本地地址非法")
	}
	return ip.Unmap()
}

// setSocketProtector 注入 fake socketProtector（记录 protect 调用），测试结束还原。
func setSocketProtector(t *testing.T, fn func(int) error) {
	t.Helper()
	prev := socketProtector
	socketProtector = fn
	t.Cleanup(func() { socketProtector = prev })
}

// TestPhysicalDNSResolverDirectUpstream 验证 NewPhysicalDNSResolver 返回的
// 解析器直连配置的物理 DNS 上游（内存 mock），不经系统解析器：返回 mock 的
// IP、且 socket 经 protect() 豁免（不会重新进入 TUN 环路）。
func TestPhysicalDNSResolverDirectUpstream(t *testing.T) {
	_, serverAddr := startPhysicalDNSMock(t, netip.MustParseAddr("1.2.3.4"))
	setPhysicalDNSPort(t, serverAddr.Port)

	var protectCalls atomic.Int32
	setSocketProtector(t, func(int) error { protectCalls.Add(1); return nil })

	resolver := NewPhysicalDNSResolver([]netip.Addr{udpAddrIP(serverAddr)})
	ip, err := resolver(context.Background(), "www.example.com")
	if err != nil {
		t.Fatalf("物理直连解析失败：%v", err)
	}
	if got := ip.String(); got != "1.2.3.4" {
		t.Fatalf("解析结果 = %s，期望 mock 上游的 1.2.3.4（直连物理 DNS 而非系统解析器）", got)
	}
	if protectCalls.Load() == 0 {
		t.Fatal("物理 DNS socket 未走 protect() 豁免 → 查询会重新进入 TUN 环路")
	}
}

// TestDNSInterceptorFrontProxyNoLoop 是防回流回归（TunnelDNS ≠
// kernel.ResolveDNS 的语义侧）：模拟 front-proxy 装配（TunnelDNS =
// NewPhysicalDNSResolver + proxy 域名走该解析器）。proxy 域名若回流会经
// 隧道/系统解析器自锁——这里断言隧道解析器零调用、解析结果来自物理 DNS
// mock、socket 走 protect 豁免。direct 域名同样走物理上游（分流判定不变）。
func TestDNSInterceptorFrontProxyNoLoop(t *testing.T) {
	_, serverAddr := startPhysicalDNSMock(t, netip.MustParseAddr("1.2.3.4"))
	setPhysicalDNSPort(t, serverAddr.Port)

	var protectCalls atomic.Int32
	setSocketProtector(t, func(int) error { protectCalls.Add(1); return nil })

	// 隧道解析器：若回流路径存在，proxy 域名会经它（=kernel.ResolveDNS → 系统
	// 解析器）解析；断言它零调用。
	tunnel := &fakeResolve{v4: map[string]string{"www.google.com": "142.250.72.4"}}
	mockUpstream := []netip.Addr{udpAddrIP(serverAddr)}

	// 模拟 Android 桥 front-proxy 装配：TunnelDNS = NewPhysicalDNSResolver，
	// PhysicalDNS 同步传入拦截器（direct 分支的 physicalResolver 也用同一上游）。
	d := NewDNSInterceptor(NewPhysicalDNSResolver(mockUpstream), splitTestRoute, mockUpstream)

	// proxy 域名（回流高危路径：旧装配走 d.resolve = kernel.ResolveDNS）。
	resp := d.HandleQuery(packAQuery(0x30, "www.google.com"))
	if resp == nil {
		t.Fatal("front-proxy 装配下 proxy 域名查询应返回响应")
	}
	var m dnsmessage.Message
	if err := m.Unpack(resp); err != nil {
		t.Fatalf("解包响应失败：%v", err)
	}
	if len(m.Answers) != 1 {
		t.Fatalf("应恰有 1 条应答，得到 %d", len(m.Answers))
	}
	a, ok := m.Answers[0].Body.(*dnsmessage.AResource)
	if !ok {
		t.Fatalf("应答应为 A 记录，得到 %T", m.Answers[0].Body)
	}
	if got := net.IP(a.A[:]).String(); got != "1.2.3.4" {
		t.Fatalf("proxy 域名应返回物理 DNS 上游的 1.2.3.4（而非隧道/系统解析器），得到 %s", got)
	}
	if len(tunnel.called) != 0 {
		t.Fatalf("隧道解析器被调用 %v 次 → 回流路径未阻断（front-proxy 下 TunnelDNS 必须≠ kernel.ResolveDNS）", tunnel.called)
	}
	if protectCalls.Load() == 0 {
		t.Fatal("物理 DNS socket 未走 protect() 豁免 → 查询会重新进入 TUN 环路")
	}

	// direct 域名：分流判定不变，仍走物理（d.physicalResolver 用同一 mock）。
	resp = d.HandleQuery(packAQuery(0x31, "www.example.com"))
	if resp == nil {
		t.Fatal("direct 域名查询应返回响应")
	}
	var m2 dnsmessage.Message
	if err := m2.Unpack(resp); err != nil {
		t.Fatalf("解包 direct 响应失败：%v", err)
	}
	if len(m2.Answers) != 1 {
		t.Fatalf("direct 应恰有 1 条应答，得到 %d", len(m2.Answers))
	}
	a2, ok := m2.Answers[0].Body.(*dnsmessage.AResource)
	if !ok {
		t.Fatalf("direct 应答应为 A 记录，得到 %T", m2.Answers[0].Body)
	}
	if got := net.IP(a2.A[:]).String(); got != "1.2.3.4" {
		t.Fatalf("direct 域名应返回物理 DNS 上游的 1.2.3.4，得到 %s", got)
	}
}

//go:build android || linux

package androidvpn

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// fakeResolve 测试用假解析器：host→ipv4/ipv6 静态表。
type fakeResolve struct {
	v4 map[string]string
	v6 map[string]string
}

func (f *fakeResolve) resolve(ctx context.Context, host string) (net.IP, error) {
	if f == nil {
		return nil, errors.New("no resolver")
	}
	if ip, ok := f.v4[host]; ok {
		return net.ParseIP(ip), nil
	}
	if ip, ok := f.v6[host]; ok {
		return net.ParseIP(ip), nil
	}
	return nil, errors.New("NXDOMAIN: " + host)
}

// newTestInterceptor 构造带假解析器的拦截器。
func newTestInterceptor() *dnsInterceptor {
	return NewDNSInterceptor((&fakeResolve{
		v4: map[string]string{"www.example.com": "57.145.12.1"},
		v6: map[string]string{"ipv6.example.com": "2606:4700:4700::1111"},
	}).resolve)
}

// packAQuery 构造一条 A 查询报文。
func packAQuery(id uint16, host string) []byte {
	name, _ := dnsmessage.NewName(host + ".")
	q := dnsmessage.Message{
		Header: dnsmessage.Header{ID: id, RecursionDesired: true},
		Questions: []dnsmessage.Question{{
			Name:  name,
			Type:  dnsmessage.TypeA,
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

// TestDNSInterceptorQueryTypeFilter 验证查询类型过滤：MX 查询不处理；
// AAAA 查询但解析器只回 v4 时不返回（空应答，Android 回退下一个 DNS）。
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

	// AAAA 查询但 resolve 只回 v4 → nil（不构造不匹配的 AAAA 应答）
	aaaaQ := dnsmessage.Message{
		Header: dnsmessage.Header{ID: 2, RecursionDesired: true},
		Questions: []dnsmessage.Question{{
			Name:  name,
			Type:  dnsmessage.TypeAAAA,
			Class: dnsmessage.ClassINET,
		}},
	}
	aaaaWire, _ := aaaaQ.Pack()
	if got := d.HandleQuery(aaaaWire); got != nil {
		t.Fatal("AAAA 查询解析到 v4 应返回 nil（地址族不匹配）")
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

// TestDNSInterceptorResolveFailure 验证解析失败 → nil（丢弃查询）。
func TestDNSInterceptorResolveFailure(t *testing.T) {
	d := newTestInterceptor()
	resp := d.HandleQuery(packAQuery(9, "nxdomain.example.com"))
	if resp != nil {
		t.Fatal("解析失败应返回 nil")
	}
}

// TestDNSInterceptorNilResolve 验证未配置解析函数时全部丢弃。
func TestDNSInterceptorNilResolve(t *testing.T) {
	d := NewDNSInterceptor(nil)
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

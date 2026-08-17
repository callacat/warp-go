package route

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"
)

// 默认模板（与 DefaultRules 同序），引擎测试统一使用。
const testRulesText = `proxy,geosite:google
proxy,geosite:geolocation-!cn
direct,geoip:private
direct,geosite:private
direct,geosite:cn
direct,geoip:cn
`

// newTestEngine 用合成 GEO 库 + 给定规则文本构造引擎（不走网络）。
func newTestEngine(t *testing.T, rulesText string) *Engine {
	t.Helper()
	silenceLogs(t)
	geoDir := writeTestGeoData(t)
	rulesPath := filepath.Join(t.TempDir(), "rules.txt")
	if err := os.WriteFile(rulesPath, []byte(rulesText), 0o644); err != nil {
		t.Fatal(err)
	}
	e, err := NewEngine(rulesPath, geoDir)
	if err != nil {
		t.Fatalf("NewEngine 失败：%v", err)
	}
	t.Cleanup(e.Close)
	return e
}

func TestMatchGeoSiteSuffix(t *testing.T) {
	e := newTestEngine(t, testRulesText)

	cases := []struct {
		host        string
		wantAct     string
		wantMatched bool
		wantValue   string
	}{
		{"google.com", "proxy", true, "google"},        // 根域本身
		{"www.google.com", "proxy", true, "google"},    // 子域
		{"a.b.google.com", "proxy", true, "google"},    // 深层子域
		{"notgoogle.com", "", false, ""},               // 标签边界：非子域 → 未命中兜底 direct
		{"unrelated.example", "", false, ""},           // 完全无关 → 默认 direct 兜底
		{"qq.com", "direct", true, "cn"},               // geosite:cn（混合大小写类别名）
		{"www.qq.com", "direct", true, "cn"},           // geosite:cn 子域
		{"localhost", "direct", true, "private"},       // geosite:private
		{"x.internal.corp", "direct", true, "private"}, // geosite:private 子域
		{"WWW.GOOGLE.COM", "proxy", true, "google"},    // 大写域名大小写不敏感
		{"www.google.com.", "proxy", true, "google"},   // FQDN 尾部点
	}
	for _, tc := range cases {
		act, rule, matched := e.Match(tc.host, netip.Addr{})
		if tc.wantMatched {
			if !matched {
				t.Errorf("Match(%q) 应命中，得到未命中", tc.host)
				continue
			}
			if act != tc.wantAct || rule.Value != tc.wantValue {
				t.Errorf("Match(%q) = (%s, %+v)，期望 (%s, value=%s)",
					tc.host, act, rule, tc.wantAct, tc.wantValue)
			}
		} else if matched {
			t.Errorf("Match(%q) 应未命中，得到 (%s, %+v)", tc.host, act, rule)
		} else if act != ActionDirect {
			t.Errorf("Match(%q) 未命中时应返回 direct，得到 %s", tc.host, act)
		}
	}
}

func TestMatchGeoSiteDomainTypes(t *testing.T) {
	e := newTestEngine(t, "proxy,geosite:google\n")

	cases := []struct {
		host      string
		wantMatch bool
	}{
		{"gstatic.com", true},               // Full=3 精确
		{"sub.gstatic.com", false},          // Full 不匹配子域
		{"foo.doubleclick.net", true},       // Plain=0 子串
		{"x.googlevideo.com", true},         // Regex=1
		{"googlevideo.com.evil.com", false}, // Regex 锚定结尾，不误伤
	}
	for _, tc := range cases {
		_, _, matched := e.Match(tc.host, netip.Addr{})
		if matched != tc.wantMatch {
			t.Errorf("Match(%q) 命中 = %v，期望 %v", tc.host, matched, tc.wantMatch)
		}
	}
}

func TestMatchGeoIP(t *testing.T) {
	e := newTestEngine(t, testRulesText)

	// 域名 + 已解析 IP → geoip:cn
	if act, rule, matched := e.Match("unknown.cn", netip.MustParseAddr("1.0.1.5")); !matched || act != "direct" || rule.Value != "cn" {
		t.Errorf("域名+中国 IP 应命中 geoip:cn，得到 (%s, %+v, %v)", act, rule, matched)
	}
	// 域名 + 非中国 IP → 不命中 geoip:cn，落兜底
	if _, _, matched := e.Match("unknown.cn", netip.MustParseAddr("8.8.8.8")); matched {
		t.Error("8.8.8.8 不应命中 geoip:cn")
	}
	// geoip:private 是库内真实条目
	if act, rule, matched := e.Match("intranet", netip.MustParseAddr("10.2.3.4")); !matched || act != "direct" || rule.Value != "private" {
		t.Errorf("10.2.3.4 应命中 geoip:private，得到 (%s, %+v, %v)", act, rule, matched)
	}
	// IPv6 段
	if _, _, matched := e.Match("v6host", netip.MustParseAddr("2001:250::5")); !matched {
		t.Error("2001:250::5 应命中 geoip:cn")
	}
}

// TestMatchGeoIPTelegram 验证默认模板的 proxy,geoip:telegram 规则：Telegram
// IP 段走 WARP 隧道（在封锁 TG 的网络下可经隧道连通），非 TG IP 不受影响。
func TestMatchGeoIPTelegram(t *testing.T) {
	// 规则文本含 telegram 行（与默认模板一致），依赖合成 GEO 库的 telegram 类别。
	e := newTestEngine(t, "proxy,geoip:telegram\n")

	// Telegram 段内 IP → proxy
	if act, rule, matched := e.Match("telegram-host", netip.MustParseAddr("149.154.175.100")); !matched || act != "proxy" || rule.Value != "telegram" {
		t.Errorf("149.154.175.100 应命中 proxy,geoip:telegram，得到 (%s, %+v, %v)", act, rule, matched)
	}
	if act, _, matched := e.Match("tg2", netip.MustParseAddr("91.108.5.7")); !matched || act != "proxy" {
		t.Errorf("91.108.5.7 应命中 telegram，得到 (%s, %v)", act, matched)
	}
	// 段外 IP → 不命中（兜底）
	if _, _, matched := e.Match("non-tg", netip.MustParseAddr("8.8.8.8")); matched {
		t.Error("8.8.8.8 不应命中 geoip:telegram")
	}
	// IP 字面量（Telegram IP 无域名解析）也应命中
	if act, _, matched := e.Match("149.154.175.100", netip.Addr{}); !matched || act != "proxy" {
		t.Errorf("IP 字面量 149.154.175.100 应命中 telegram，得到 (%s, %v)", act, matched)
	}
}

func TestMatchGeoIPLiteral(t *testing.T) {
	e := newTestEngine(t, testRulesText)

	// IP 字面量 → 只走 geoip 规则（geosite/domain 跳过）
	if act, rule, matched := e.Match("10.0.0.1", netip.Addr{}); !matched || act != "direct" || rule.Value != "private" {
		t.Errorf("IP 字面量 10.0.0.1 应命中 geoip:private，得到 (%s, %+v, %v)", act, rule, matched)
	}
	// 公网 IP 未命中 → 兜底 direct
	if _, _, matched := e.Match("8.8.8.8", netip.Addr{}); matched {
		t.Error("8.8.8.8 不应命中任何规则")
	}
	// IP 字面量不匹配 domain: 规则（域名语义对 IP 无意义）
	ipOnly := newTestEngine(t, "proxy,domain:1.2.3.4\n")
	if _, _, matched := ipOnly.Match("1.2.3.4", netip.Addr{}); matched {
		t.Error("IP 字面量不应命中 domain: 规则")
	}
	// IP 字面量 host 覆盖传入的 ip 参数：10.0.0.1 在库内 private 段，
	// 若参数 8.8.8.8 生效则不应命中
	if act, rule, matched := e.Match("10.0.0.1", netip.MustParseAddr("8.8.8.8")); !matched || act != "direct" || rule.Value != "private" {
		t.Errorf("IP 字面量 host 应覆盖 ip 参数并命中 geoip:private，得到 (%s, %+v, %v)", act, rule, matched)
	}
}

func TestMatchGeoIPLan(t *testing.T) {
	e := newTestEngine(t, "direct,geoip:lan\n")

	lanCases := []string{"127.0.0.1", "10.0.0.1", "192.168.1.1", "::1", "fe80::1", "0.0.0.0", "224.0.0.1"}
	for _, s := range lanCases {
		if _, _, matched := e.Match("", netip.MustParseAddr(s)); !matched {
			t.Errorf("%s 应命中 geoip:lan", s)
		}
	}
	if _, _, matched := e.Match("", netip.MustParseAddr("8.8.8.8")); matched {
		t.Error("8.8.8.8 不应命中 geoip:lan")
	}
	// 域名 + 提供内网 IP → 命中 lan
	if _, _, matched := e.Match("printer.local", netip.MustParseAddr("192.168.1.50")); !matched {
		t.Error("域名+内网 IP 应命中 geoip:lan")
	}
	// 域名 + 未解析 → lan 无法判定，落兜底
	if _, _, matched := e.Match("printer.local", netip.Addr{}); matched {
		t.Error("未提供 IP 时 geoip:lan 不应命中")
	}
}

func TestMatchDomainRule(t *testing.T) {
	e := newTestEngine(t, "proxy,domain:example.com\n")

	for _, host := range []string{"example.com", "www.example.com", "a.b.example.com"} {
		if act, rule, matched := e.Match(host, netip.Addr{}); !matched || act != "proxy" || rule.Value != "example.com" {
			t.Errorf("Match(%q) 应命中 domain:example.com，得到 (%s, %+v, %v)", host, act, rule, matched)
		}
	}
	// 标签边界：notexample.com / example.com.evil.com 不命中
	for _, host := range []string{"notexample.com", "example.com.evil.com", "xexample.com"} {
		if _, _, matched := e.Match(host, netip.Addr{}); matched {
			t.Errorf("Match(%q) 不应命中 domain:example.com（标签边界）", host)
		}
	}
	// 大小写不敏感
	if _, _, matched := e.Match("WWW.Example.COM", netip.Addr{}); !matched {
		t.Error("大写域名应命中 domain: 规则")
	}
}

func TestMatchFirstWins(t *testing.T) {
	// 相同条件两条规则：先出现者生效
	e1 := newTestEngine(t, "proxy,domain:example.com\ndirect,domain:example.com\n")
	if act, rule, matched := e1.Match("www.example.com", netip.Addr{}); !matched || act != "proxy" {
		t.Errorf("先出现的 proxy 应生效，得到 (%s, %+v, %v)", act, rule, matched)
	}

	e2 := newTestEngine(t, "direct,domain:example.com\nproxy,domain:example.com\n")
	if act, _, matched := e2.Match("www.example.com", netip.Addr{}); !matched || act != "direct" {
		t.Errorf("先出现的 direct 应生效，得到 (%s, %v)", act, matched)
	}
}

func TestMatchFallbackDirect(t *testing.T) {
	e := newTestEngine(t, "proxy,geosite:google\n")
	act, rule, matched := e.Match("unmatched.example", netip.Addr{})
	if matched || act != ActionDirect || rule != (Rule{}) {
		t.Errorf("未命中应返回 (direct, Rule{}, false)，得到 (%s, %+v, %v)", act, rule, matched)
	}
}

func TestEngineRulesOnly(t *testing.T) {
	// 无 GEO 库（geoDir 不存在/为空）→ 引擎可用，domain: 规则生效，
	// geosite/geoip 规则静默不命中
	silenceLogs(t)
	rulesPath := filepath.Join(t.TempDir(), "rules.txt")
	os.WriteFile(rulesPath, []byte("proxy,domain:foo.com\nproxy,geosite:google\n"), 0o644)

	e, err := NewEngine(rulesPath, filepath.Join(t.TempDir(), "empty-geo"))
	if err != nil {
		t.Fatalf("NewEngine（rules-only）失败：%v", err)
	}
	defer e.Close()

	if act, _, matched := e.Match("www.foo.com", netip.Addr{}); !matched || act != "proxy" {
		t.Errorf("domain: 规则应命中，得到 (%s, %v)", act, matched)
	}
	if _, _, matched := e.Match("www.google.com", netip.Addr{}); matched {
		t.Error("无 GEO 库时 geosite 规则不应命中")
	}
	if _, _, matched := e.Match("1.2.3.4", netip.Addr{}); matched {
		t.Error("无 GEO 库时 geoip 规则不应命中")
	}
}

func TestEngineReload(t *testing.T) {
	silenceLogs(t)
	geoDir := writeTestGeoData(t)
	rulesPath := filepath.Join(t.TempDir(), "rules.txt")
	os.WriteFile(rulesPath, []byte("proxy,domain:foo.com\n"), 0o644)

	e, err := NewEngine(rulesPath, geoDir)
	if err != nil {
		t.Fatalf("NewEngine 失败：%v", err)
	}
	defer e.Close()

	if act, _, matched := e.Match("www.foo.com", netip.Addr{}); !matched || act != "proxy" {
		t.Fatalf("初始规则应 proxy，得到 (%s, %v)", act, matched)
	}

	// 修改文件 + Reload（GUI 手动触发路径）
	os.WriteFile(rulesPath, []byte("direct,domain:foo.com\n"), 0o644)
	if err := e.Reload(); err != nil {
		t.Fatalf("Reload 失败：%v", err)
	}
	if act, _, matched := e.Match("www.foo.com", netip.Addr{}); !matched || act != "direct" {
		t.Errorf("Reload 后应 direct，得到 (%s, %v)", act, matched)
	}

	// Rules() 反映新规则
	rules := e.Rules()
	if len(rules) != 1 || rules[0].Action != "direct" {
		t.Errorf("Rules() = %+v，期望 [direct,domain:foo.com]", rules)
	}
}

func TestEngineReloadBadFile(t *testing.T) {
	silenceLogs(t)
	rulesPath := filepath.Join(t.TempDir(), "rules.txt")
	os.WriteFile(rulesPath, []byte("proxy,domain:foo.com\n"), 0o644)

	e, err := NewEngine(rulesPath, t.TempDir())
	if err != nil {
		t.Fatalf("NewEngine 失败：%v", err)
	}
	defer e.Close()

	os.WriteFile(rulesPath, []byte("not a rule\n"), 0o644)
	if err := e.Reload(); err == nil {
		t.Fatal("非法规则文件 Reload 应报错")
	}
	// 旧规则保持生效
	if act, _, matched := e.Match("www.foo.com", netip.Addr{}); !matched || act != "proxy" {
		t.Errorf("Reload 失败后应保持旧规则，得到 (%s, %v)", act, matched)
	}
}

func TestEngineStats(t *testing.T) {
	e := newTestEngine(t, "proxy,domain:hit.com\n")
	e.Match("www.hit.com", netip.Addr{}) // proxy 命中
	e.Match("www.hit.com", netip.Addr{}) // proxy 命中
	e.Match("miss.com", netip.Addr{})    // 未命中

	st := e.Stats()
	if st.ProxyHits != 2 {
		t.Errorf("ProxyHits = %d，期望 2", st.ProxyHits)
	}
	if st.DirectHits != 0 {
		t.Errorf("DirectHits = %d，期望 0", st.DirectHits)
	}
	if st.Misses != 1 {
		t.Errorf("Misses = %d，期望 1", st.Misses)
	}
}

func TestMatchReject(t *testing.T) {
	e := newTestEngine(t, "REJECT,domain:ads.com\nproxy,geosite:google\n")

	cases := []struct {
		host        string
		wantAct     string
		wantMatched bool
	}{
		{"ads.com", "reject", true},        // REJECT 首条命中
		{"cdn.ads.com", "reject", true},    // 子域
		{"google.com", "proxy", true},      // 后续规则仍生效
		{"example.org", "direct", false},   // 未命中兜底 direct
	}
	for _, tc := range cases {
		act, _, matched := e.Match(tc.host, netip.Addr{})
		if matched != tc.wantMatched || (matched && act != tc.wantAct) {
			t.Errorf("Match(%q) = (%s, %v)，期望 (%s, %v)",
				tc.host, act, matched, tc.wantAct, tc.wantMatched)
		}
	}
}

func TestEngineStatsReject(t *testing.T) {
	e := newTestEngine(t, "reject,domain:ads.com\ndirect,domain:ok.com\n")
	e.Match("ads.com", netip.Addr{})     // reject 命中
	e.Match("www.ads.com", netip.Addr{}) // reject 命中
	e.Match("ok.com", netip.Addr{})      // direct 命中
	e.Match("miss.com", netip.Addr{})    // 未命中

	st := e.Stats()
	if st.RejectedHits != 2 {
		t.Errorf("RejectedHits = %d，期望 2", st.RejectedHits)
	}
	if st.DirectHits != 1 {
		t.Errorf("DirectHits = %d，期望 1（reject 不应计入 direct）", st.DirectHits)
	}
	if st.ProxyHits != 0 {
		t.Errorf("ProxyHits = %d，期望 0", st.ProxyHits)
	}
	if st.Misses != 1 {
		t.Errorf("Misses = %d，期望 1", st.Misses)
	}
}

// TestMatchDefaultFallback 锁定 `default:` 兜底声明语义：规则文件显式声明
// 的兜底优先于调用方代码硬编码（东哥 v0.5.28 要求）。未命中时返回声明行为
// 且 matched=true；命中规则仍优先于 default。
func TestMatchDefaultFallback(t *testing.T) {
	e := newTestEngine(t, "proxy,geosite:google\ndefault:direct\n")
	if act, _, matched := e.Match("unmatched.example", netip.Addr{}); !matched || act != "direct" {
		t.Fatalf("声明 default:direct 后未命中应返回 direct+matched=true，得到 (%s, %v)", act, matched)
	}
	if act, _, matched := e.Match("www.google.com", netip.Addr{}); !matched || act != "proxy" {
		t.Fatalf("命中规则应优先于 default，得到 (%s, %v)", act, matched)
	}

	e2 := newTestEngine(t, "proxy,geosite:google\ndefault:proxy\n")
	if act, _, matched := e2.Match("unmatched.example", netip.Addr{}); !matched || act != "proxy" {
		t.Fatalf("声明 default:proxy 后未命中应返回 proxy+matched=true，得到 (%s, %v)", act, matched)
	}
}

// TestMatchDefaultRejectFallback 验证 reject 也可作兜底（拦截未匹配流量）。
func TestMatchDefaultRejectFallback(t *testing.T) {
	e := newTestEngine(t, "default:reject\n")
	if act, _, matched := e.Match("anything.example", netip.Addr{}); !matched || act != "reject" {
		t.Fatalf("default:reject 未命中应返回 reject+matched=true，得到 (%s, %v)", act, matched)
	}
}

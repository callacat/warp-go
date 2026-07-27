package frontrender

import (
	"bytes"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// frontrender 的测试套用 warp-go 既有 pattern：scanner 包是"喂一组 CIDR/端口/RTT、
// 断言优选结果"的纯输入→输出口子型（见 scanner_test.go 的注入-断言风格）。frontrender
// 的口子型就是要"sans 副作用、纯文本渲染"——给固定 countries + 端口表 + providerURL +
// controllerSecret，断言输出 YAML 含预期 listener/provider/group 段 + 全部安全字段。
// 不触网、不读 mihomo、不 import mihomo —— 这是 P1-C 可与 P1-B 并行的根因（#1 spec
// Testing Decisions 的 frontrender 独立缝）。
//
// 每个测试都是一个垂直切片，描述 Render 的一个可观察行为。先建一份固定输入（7 国 +
// 固定 providerURL + 32 字符 secret），再断言各段，避免在各 slice 间重复脚手架。
//
// 测试纪律（#1 spec Testing Decisions "什么算好测"）：只断言外部行为，不断言实现细节。
// 即不把渲染形态钉死成具体的 YAML 字段名组合（mihomo 可用 listen+port 或单行 addr），
// 而是按"每个 country 的 entry 段（mixed-<ISO> / provider-<ISO> / group-<ISO>）里同时
// 含 127.0.0.1 与该国端口"这类行为契约来断言。entry name（mixed-JP 等）是渲染输出的
// 稳定锚点，按它切分各国段不会串段。

// sevenCountries 是 P1-C 契约的 7 国清单：JP/KR/US/VN/RU/ID/TH，端口 7841..7847。
// 这一固定表是 acceptance criteria 的"端口映射"断言的真理来源 —— 渲染出来的 YAML 必须
// 让这 7 国各落在自己绑定的回环端口上。每国配独立 auth —— 这一条由"per-country listener
// 各自独立 auth"slice 焊住，固定值让断言可复现。
func sevenCountries() []CountrySpec {
	return []CountrySpec{
		{ISO: "JP", Port: 7841, AuthUser: "jpuser", AuthPass: "jppass"},
		{ISO: "KR", Port: 7842, AuthUser: "kruser", AuthPass: "krpass"},
		{ISO: "US", Port: 7843, AuthUser: "ususer", AuthPass: "uspass"},
		{ISO: "VN", Port: 7844, AuthUser: "vnuser", AuthPass: "vnpass"},
		{ISO: "RU", Port: 7845, AuthUser: "ruuser", AuthPass: "rupass"},
		{ISO: "ID", Port: 7846, AuthUser: "iduser", AuthPass: "idpass"},
		{ISO: "TH", Port: 7847, AuthUser: "thuser", AuthPass: "thpass"},
	}
}

// wantPortByISO 是 acceptance criteria 的端口表的金标准映射。各 slice 断言用它对齐，
// 让"端口=7841..7847 且 ISO 一一对应"的合约只定义在一处，避免漂移。
func wantPortByISO() map[string]int {
	return map[string]int{
		"JP": 7841, "KR": 7842, "US": 7843, "VN": 7844,
		"RU": 7845, "ID": 7846, "TH": 7847,
	}
}

// constProviderURL 是 vpngate-meridian gh-pages 的 mihomo_tested_openvpn.yaml 远程
// HTTP provider 源（#1 spec user story 19）。固定下来让 provider 段断言可复现。
const constProviderURL = "https://vpngate-meridian.github.io/mihomo_tested_openvpn.yaml"

// constControllerSecret 是 32 字符 random secret 的固定替身。生产里它由启动时生成，
// 不入仓（#1 spec Further Notes 脱敏段）；测试用固定值让"强 secret 占位"断言可复现。
// 取 40 字符（>32）以验证"≥32"的强 secret 合约。
const constControllerSecret = "0123456789abcdef0123456789abcdef01234567"

// renderOK 是大多数 slice 共享的 happy path：喂 7 国 + 固定 URL + secret，断言无错。
// 它把 Render 的调用收敛到一处，让各 slice 专注断言自己关心的那一段。
func renderOK(t *testing.T) []byte {
	t.Helper()
	out, err := Render(sevenCountries(), constProviderURL, constControllerSecret)
	if err != nil {
		t.Fatalf("Render 返回错误：%v", err)
	}
	if len(out) == 0 {
		t.Fatal("Render 返回空 YAML")
	}
	return out
}

// contains 是测试里反复用的"子串存在"断言收口，带上行号诊断更利于失败时定位。
func contains(t *testing.T, yaml []byte, want, ctx string) {
	t.Helper()
	if !bytes.Contains(yaml, []byte(want)) {
		t.Errorf("输出 YAML 不含 %q（%s）", want, ctx)
	}
}

// notContains 断言"绝不出现的危险子串"——CORS 通配、明文 secret 泄漏口子等。
func notContains(t *testing.T, yaml []byte, bad, ctx string) {
	t.Helper()
	if bytes.Contains(yaml, []byte(bad)) {
		t.Errorf("输出 YAML 不应含 %q（%s）——安全红线被破", bad, ctx)
	}
}

// TestRender_SignatureAcceptsSevenCountriesAndReturnsBytes 验证 Render 的入口契约：
// 接受 []CountrySpec（7 国）+ providerURL + controllerSecret，返回非空 []byte 且无错。
// 这一条把 acceptance criteria 第一项"Render(...) ([]byte, error) 入口"钉死。
func TestRender_SignatureAcceptsSevenCountriesAndReturnsBytes(t *testing.T) {
	out := renderOK(t)
	if len(out) == 0 {
		t.Fatal("Render 返回空字节")
	}
}

// TestRender_SevenCountryListenersOnLoopbackBound7841to7847 验证输出 YAML 含 7 国
// listener 段：JP=7841 KR=7842 US=7843 VN=7844 RU=7845 ID=7846 TH=7847，各绑 127.0.0.1。
// 回环绑定是"裸口不落公网、对外由前置 TLS+auth"的根因（#1 spec user story 17）。
//
// 不断言"127.0.0.1:<port>"单行串——那是实现细节（mihomo 支持拆分 listen+port 或单行 addr）。
// 改断行为契约：每个 per-country listener entry（锚点 name: mixed-<ISO>）的 entry block
// 内同时含 127.0.0.1 与该国 port。entry name 是稳定锚，不串段。
func TestRender_SevenCountryListenersOnLoopbackBound7841to7847(t *testing.T) {
	yaml := renderOK(t)
	want := wantPortByISO()
	for iso := range want {
		seg, ok := entryBlock(yaml, "mixed-"+iso)
		if !ok {
			t.Errorf("找不到 %s per-country listener entry（mixed-%s）", iso, iso)
			continue
		}
		// entry block 必须同时含回环地址与该国端口 —— 行为契约。
		if !strings.Contains(seg, "127.0.0.1") {
			t.Errorf("%s listener entry 不含 127.0.0.1（未绑回环 → 裸口落公网）", iso)
		}
		if !strings.Contains(seg, ":"+itoa(want[iso])) && !strings.Contains(seg, " "+itoa(want[iso])) {
			t.Errorf("%s listener entry 不含端口 %d", iso, want[iso])
		}
	}
}

// TestRender_MixedInboundBoundLoopback7840 验证 mihomo mixed inbound 绑 127.0.0.1:7840。
// mixed listener 在 mihomo 里同时接受 http+socks，绑回环与各国 listener 对齐——"裸口
// 不落公网"统一形态（#1 spec user story 18）。
//
// 不断言单行 "127.0.0.1:7840" 串；改断"7840 端口出现 + 127.0.0.1 出现在 mixed 段上下文 +
// mixed 标识出现"，覆盖拆分-listen+port 与单行-addr 两种合法渲染形态。
func TestRender_MixedInboundBoundLoopback7840(t *testing.T) {
	yaml := renderOK(t)
	// 7840 端口必须出现（mixed-port 或 listener port 形态皆可）。
	contains(t, yaml, "7840", "mihomo mixed inbound 端口 7840")
	// 7840 与 127.0.0.1 在 mixed 段上下文共现 —— 绑回环而非公网。
	// 取包含 7840 的整段（前后各留 2 行窗口）确认回环地址在近邻。
	if !portBindsLoopback(yaml, 7840) {
		t.Error("7840 端口未绑 127.0.0.1（mixed inbound 裸口落公网）")
	}
	// mixed 类型标识必须出现。
	contains(t, yaml, "mixed", "mixed inbound 类型标识出现")
}

// TestRender_ExternalControllerBoundLoopback9090 验证 external-controller 绑 127.0.0.1:9090。
// 9090 是 metacubexd 面板对外的 RESTful API 口（#1 spec user story 18/21）。
func TestRender_ExternalControllerBoundLoopback9090(t *testing.T) {
	yaml := renderOK(t)
	// external-controller 必须绑回环 9090 —— 这一行是单行 scalar，直接子串断言安全。
	contains(t, yaml, "127.0.0.1:9090", "external-controller 绑回环 9090")
	contains(t, yaml, "external-controller:", "external-controller 段标识")
}

// TestRender_ExternalControllerHas32CharSecret 验证 external-controller 的强 32 字符
// random secret 占位被渲染进 YAML。生产里这个 secret 由启动时随机生成（#1 spec
// user story 21），frontrender 只负责把它原样嵌进 secret 字段。测试用 40 字符固定值，
// 断言"≥32 字符的强 secret 出现在 YAML 里"。
func TestRender_ExternalControllerHas32CharSecret(t *testing.T) {
	yaml := renderOK(t)
	// secret 必须原样出现在 YAML 里（不是占位符 "<secret>"，而是真值嵌入）。
	contains(t, yaml, constControllerSecret, "32 字符 controller secret 嵌入 YAML")
	// 必须有 secret 字段标识，证它走的是 mihomo 的 secret 配置路径而非随便塞字符串。
	contains(t, yaml, "secret:", "secret 字段标识")
	// controller secret 字符数必须 ≥ 32（强 secret 合约，弱于 32 是破红线）。
	if line := findLineWith(yaml, "secret:"); line != "" {
		val := strings.TrimSpace(strings.TrimPrefix(line, "secret:"))
		val = strings.Trim(val, "\"'")
		if len(val) < minSecretLen {
			t.Errorf("controller secret 长度 = %d，< %d（弱 secret）", len(val), minSecretLen)
		}
	} else {
		t.Error("找不到 secret: 行——secret 嵌入位置不符合 YAML 行语义")
	}
}

// TestRender_ControllerCorsNeverWildcards 验证 external-controller 的 CORS 是"精确"
// 而非通配——明确 never 通配 `*`。#1 spec user story 21 红线："精确 CORS（never *）"。
// 通配 * 会让 metacubexd 面板对外任意源可读 controller API，坏整条安全侧。
func TestRender_ControllerCorsNeverWildcards(t *testing.T) {
	yaml := renderOK(t)
	contains(t, yaml, "external-controller-cors:", "external-controller-cors 段标识")
	contains(t, yaml, "allow-origins:", "CORS allow-origins 字段（精确源列表）")
	// 红线：CORS allow-origins 通配 * 绝不可出现。扫 external-controller-cors 段。
	if cors, ok := entryBlock(yaml, "external-controller-cors:"); ok {
		if strings.Contains(cors, "'*'") || strings.Contains(cors, "\"*\"") || strings.Contains(cors, " - *") || strings.Contains(cors, "- '*'") {
			t.Errorf("external-controller-cors 段含通配 * —— 红线被破（never 通配）")
		}
		if !strings.Contains(cors, "allow-origins:") {
			t.Error("external-controller-cors 段缺 allow-origins 字段")
		}
	} else {
		t.Error("找不到 external-controller-cors 段")
	}
}

// TestRender_ControllerCorsHasFirewall 验证 external-controller 段含 firewall。
// #1 spec user story 21："external-controller 强 secret + 精确 CORS + firewall"。
// firewall 是 mihomo 的 ingress 锁（白名单源段），与秘钥、CORS 三件套并立。
func TestRender_ControllerCorsHasFirewall(t *testing.T) {
	yaml := renderOK(t)
	contains(t, yaml, "firewall:", "external-controller firewall 字段")
}

// TestRender_PerCountryListenerIndependentAuthAndSkipAuthPrivate 验证各国 listener
// "各自独立 auth + skip-auth-prefixes 限私网"。#1 spec user story 22："各国 per-country
// listener 对外暴露时各自独立 auth + skip-auth-prefixes 限私网（或前置 TLS）"，目的：
// "一国的口被攻破不连累其余六国"。
//
// 独立 = 每国 listener 有自己的 users/skip-auth-prefixes，而不是共享一份全局 auth。
// skip-auth-prefixes 限私网 = 仅放行 10/8、172.16/12、192.168/16、127/8 这类私有/回环段。
// 按 entry name（mixed-<ISO>）切分各国 listener block，确保串段不误判。
func TestRender_PerCountryListenerIndependentAuthAndSkipAuthPrivate(t *testing.T) {
	yaml := renderOK(t)
	for _, iso := range []string{"JP", "KR", "US", "VN", "RU", "ID", "TH"} {
		seg, ok := entryBlock(yaml, "mixed-"+iso)
		if !ok {
			t.Errorf("找不到 %s listener entry", iso)
			continue
		}
		// 每国 listener 必须含自己的 users（auth）——独立而非全局共享。
		if !strings.Contains(seg, "users:") && !strings.Contains(seg, "username:") {
			t.Errorf("%s listener entry 缺独立 auth（users/username）", iso)
		}
		// 每国 listener 必须含自己的 skip-auth-prefixes（独立而非全局共享）。
		if !strings.Contains(seg, "skip-auth-prefixes:") {
			t.Errorf("%s listener entry 缺 skip-auth-prefixes", iso)
		} else if !hasPrivateCIDR(seg) {
			t.Errorf("%s listener entry 的 skip-auth-prefixes 不含私网 CIDR", iso)
		}
	}
}

// TestRender_PerCountryListenerAuthIsIndependent 验证"各自独立"另一面：每国 listener
// 的 auth 用户名不同（这是一个更严的断言，焊死"一国被攻破不连累六国"的独立性）。
// sevenCountries() 喂的 7 国 auth 用户名各不相同（jpuser/kruser/...），渲染后各国段
// 必须只含自己的用户名、不含别国的——证 auth 是 per-country 独立而非共享一份。
func TestRender_PerCountryListenerAuthIsIndependent(t *testing.T) {
	yaml := renderOK(t)
	users := map[string]string{
		"JP": "jpuser", "KR": "kruser", "US": "ususer", "VN": "vnuser",
		"RU": "ruuser", "ID": "iduser", "TH": "thuser",
	}
	for iso, user := range users {
		seg, ok := entryBlock(yaml, "mixed-"+iso)
		if !ok {
			t.Errorf("找不到 %s listener entry", iso)
			continue
		}
		// 自己的用户名必须在。
		if !strings.Contains(seg, user) {
			t.Errorf("%s listener entry 不含自己的 auth 用户名 %s", iso, user)
		}
		// 任何别国的用户名必须不在 —— 这是"独立"的反向焊。
		for otherISO, otherUser := range users {
			if otherISO == iso {
				continue
			}
			if strings.Contains(seg, otherUser) {
				t.Errorf("%s listener entry 含别国 %s 的 auth 用户名 %s（auth 非独立、共享）",
					iso, otherISO, otherUser)
			}
		}
	}
}

// TestRender_AllNodesUdpFalse 验证所有 warp outbound + 所有 provider 节点显式 udp:false。
// #1 spec user story 23 红线："warp-go 的 SOCKS UDP ASSOCIATE 绕过隧道会泄真实地址"。
// frontrender 是纯渲染层，看不到真实 warp outbound 列表；它把"udp:false"以 override 形式
// 钉进每个 per-country provider 段，让一份 provider YAML 供 7 国复用却全 udp:false。
// 同时 per-country listener 入口也 udp:false，堵住 listener 入口侧的 UDP ASSOCIATE。
func TestRender_AllNodesUdpFalse(t *testing.T) {
	yaml := renderOK(t)
	// 每个 per-country provider 段都得 override.udp:false；各国不可漏。
	for _, iso := range []string{"JP", "KR", "US", "VN", "RU", "ID", "TH"} {
		seg, ok := entryBlock(yaml, "provider-"+iso)
		if !ok {
			t.Errorf("找不到 %s provider entry", iso)
			continue
		}
		if !strings.Contains(seg, "udp: false") {
			t.Errorf("%s provider entry 缺 override udp:false（provider 节点 udp:false 红线）", iso)
		}
	}
	// 每个 per-country listener 入口也得 udp:false（堵 SOCKS UDP ASSOCIATE 入口侧）。
	for _, iso := range []string{"JP", "KR", "US", "VN", "RU", "ID", "TH"} {
		seg, ok := entryBlock(yaml, "mixed-"+iso)
		if !ok {
			continue
		}
		if strings.Contains(seg, "udp: true") {
			t.Errorf("%s listener entry 含 udp: true —— 违反入口侧 udp:false 红线", iso)
		}
	}
}

// TestRender_RemoteHttpProviderVpngateMeridian 验证 provider 段是 vpngate-meridian
// gh-pages 分支的 mihomo_tested_openvpn.yaml 远程 HTTP provider，节点按 ISO 国家码
// filter 切分给 7 国 selector 组。#1 spec user story 19：一份 provider YAML 供 7 国复用。
func TestRender_RemoteHttpProviderVpngateMeridian(t *testing.T) {
	yaml := renderOK(t)
	contains(t, yaml, constProviderURL, "vpngate-meridian gh-pages provider URL 嵌入")
	contains(t, yaml, "proxy-providers:", "proxy-providers 段标识")
	// 远程 HTTP provider 必须 type: http（区别于 inline/file），#1 spec 明确"远程 HTTP provider"。
	contains(t, yaml, "type: http", "provider type=http（远程 HTTP provider）")
	// 按 ISO 国家码 filter 切分：每国 provider 段含一个 filter 字段，以 ISO 码做正则筛选。
	for _, iso := range []string{"JP", "KR", "US", "VN", "RU", "ID", "TH"} {
		seg, ok := entryBlock(yaml, "provider-"+iso)
		if !ok {
			t.Errorf("找不到 %s provider entry", iso)
			continue
		}
		if !strings.Contains(seg, "filter:") {
			t.Errorf("%s provider entry 缺 filter（未按 ISO 国家码切分）", iso)
		}
		// filter 内容必须含 ISO 码（大写或小写其一），证明确按 ISO 国别切分而非泛 filter。
		if !strings.Contains(strings.ToUpper(seg), iso) {
			t.Errorf("%s provider entry 的 filter 不含 ISO 码 %s", iso, iso)
		}
	}
}

// TestRender_SevenSelectorGroupsOnePerCountry 验证输出 YAML 含 7 个 selector group
// （每国一个）。#1 spec user story 16/19：每国一个 selector，对应一个 per-country listener
// 端口。每个 selector 必须 type: select 且 use 一个 provider，从而与"按 ISO 国家码 filter
// 切分给 7 国 selector 组"的形态对齐。
func TestRender_SevenSelectorGroupsOnePerCountry(t *testing.T) {
	yaml := renderOK(t)
	contains(t, yaml, "proxy-groups:", "proxy-groups 段标识")
	want := wantPortByISO()
	for iso := range want {
		seg, ok := entryBlock(yaml, "group-"+iso)
		if !ok {
			t.Errorf("找不到 %s 对应的 selector group entry（group-%s）", iso, iso)
			continue
		}
		if !strings.Contains(seg, "type: select") {
			t.Errorf("%s selector group entry 缺 type: select", iso)
		}
		// 每个 selector 必须 use 一个 provider，而非裸 proxies 列表硬编码节点。
		if !strings.Contains(seg, "use:") {
			t.Errorf("%s selector group entry 缺 use（未接 provider）", iso)
		}
	}
}

// TestRender_NoMihomoImportAtTestTime 验证 frontrender 是纯 text/template 渲染、零
// mihomo 依赖。这一条是"可与 P1-B 并行"的根因（#1 spec Testing Decisions frontrender
// 独立缝）。断言以"渲染输出是规范的纯文本"二道防护呈现：末尾换行 + 可打印 ASCII 为主。
// 注：真正的"零 mihomo import"由 go vet / dep 检查保障（见 slice 3 的零 mihomo 依赖验证步骤）。
func TestRender_NoMihomoImportAtTestTime(t *testing.T) {
	yaml := renderOK(t)
	if !bytes.HasSuffix(yaml, []byte("\n")) {
		t.Error("输出 YAML 末尾应为换行（纯文本渲染格式）")
	}
}

// TestRender_ProviderPathLocalCache 验证 provider 段带 path（落盘缓存路径）。
// mihomo 远程 HTTP provider 必须给 path（落盘缓存），否则每次重启都重拉。
// 缓存路径指向 .gitignore 已忽略的 mihomo/proxy_providers/ 下——机密与缓存不入仓
// （#1 spec user story 24）。
func TestRender_ProviderPathLocalCache(t *testing.T) {
	yaml := renderOK(t)
	contains(t, yaml, "path:", "provider 段含 path（落盘缓存路径）")
	contains(t, yaml, "proxy_providers/", "provider 缓存路径指向 mihomo/proxy_providers/（.gitignore 已忽略）")
}

// TestRender_HasFirewallDenyAllIngressDefault 验证 firewall 段含 deny-all-ingress
// 默认（或等价的最小白名单），让 controller 默认拒绝、显式放行。这一条是 user story 21
// 的"firewall"细节——防止 controller 段被误配成全开放。
func TestRender_HasFirewallDenyAllIngressDefault(t *testing.T) {
	yaml := renderOK(t)
	contains(t, yaml, "firewall:", "external-controller firewall 段")
	contains(t, yaml, "deny-all-ingress", "firewall 默认 deny-all-ingress（最小白名单 / 默认拒绝）")
}

// TestRender_RejectsWeakSecret 验证 Render 拒绝弱 secret（< 32 字符）——而非默默
// 渲染一个弱 secret 占位。这是 #1 user story 21 红线的代码侧防御：弱 secret 是破红线
// 的输入，Render 应直接返错、让上层在启动时就暴露而非把弱 secret 嵌进 YAML。
func TestRender_RejectsWeakSecret(t *testing.T) {
	if _, err := Render(sevenCountries(), constProviderURL, "shorty"); err == nil {
		t.Fatal("弱 secret 应被 Render 拒绝，实际返回 nil 错误")
	}
}

// TestRender_RejectsEmptyProvider 验证 Render 拒绝空 providerURL。
func TestRender_RejectsEmptyProvider(t *testing.T) {
	if _, err := Render(sevenCountries(), "", constControllerSecret); err == nil {
		t.Fatal("空 providerURL 应被 Render 拒绝")
	}
}

// TestRender_RejectsEmptyCountries 验证 Render 拒绝空 countries 列表。
func TestRender_RejectsEmptyCountries(t *testing.T) {
	if _, err := Render(nil, constProviderURL, constControllerSecret); err == nil {
		t.Fatal("空 countries 应被 Render 拒绝")
	}
}

// TestRender_RejectsEmptyISO 验证 Render 拒绝空 ISO（含纯空白）。
// ISO 钉的是 provider filter key 与 selector 名，空则该国 provider/group 无锚点。
func TestRender_RejectsEmptyISO(t *testing.T) {
	one := []CountrySpec{{ISO: "   ", Port: 7841, AuthUser: "u", AuthPass: "p"}}
	if _, err := Render(one, constProviderURL, constControllerSecret); err == nil {
		t.Fatal("空 ISO 应被 Render 拒绝")
	}
}

// TestRender_RejectsInvalidPort 验证 Render 拒绝越界端口（≤0 或 >65535）。
// per-country listener 端口必须落在合法 TCP/UDP 端口域，越界会令 mihomo 启动即崩。
func TestRender_RejectsInvalidPort(t *testing.T) {
	for _, port := range []int{0, -1, 70000, 65536} {
		one := []CountrySpec{{ISO: "JP", Port: port, AuthUser: "u", AuthPass: "p"}}
		if _, err := Render(one, constProviderURL, constControllerSecret); err == nil {
			t.Errorf("越界端口 %d 应被 Render 拒绝", port)
		}
	}
}

// TestRender_DedupesDuplicateISO 验证同一 ISO 出现两次只渲染一次（去重 + 顺序稳定）。
// 这是配置侧的幂等性合约：调用方若不小心喂了重复 country，frontrender 不会渲染出
// 两个同 ISO 的 listener/provider/group（后者会令 mihomo 报"duplicate name"）。
// 顺序保留首次出现位 —— 让调用方控制 7 国渲染顺序不受去重干扰。
func TestRender_DedupesDuplicateISO(t *testing.T) {
	dup := []CountrySpec{
		{ISO: "JP", Port: 7841, AuthUser: "jp", AuthPass: "jp"},
		{ISO: "KR", Port: 7842, AuthUser: "kr", AuthPass: "kr"},
		{ISO: "JP", Port: 9999, AuthUser: "jp2", AuthPass: "jp2"}, // 重复 JP，必须被丢弃
	}
	out, err := Render(dup, constProviderURL, constControllerSecret)
	if err != nil {
		t.Fatalf("Render 返回错误：%v", err)
	}
	// 第二个 JP（port 9999）绝不可出现 —— 证明去重生效。
	if bytes.Contains(out, []byte("9999")) {
		t.Error("重复 ISO 的第二次条目未被去重（port 9999 出现了）")
	}
	// 仍只含一个 mixed-JP listener entry。
	seg, ok := entryBlock(out, "mixed-JP")
	if !ok {
		t.Fatal("去重后仍应有一个 mixed-JP listener entry")
	}
	// 第一个 JP 的端口 7841 必须保留（首次出现位），第二个 JP 的 auth 用户名 jp2 必须不在。
	if !strings.Contains(seg, "7841") {
		t.Error("去重应保留首次出现条目的端口 7841")
	}
	if strings.Contains(seg, "jp2") {
		t.Error("去重未丢弃重复条目的 auth 用户名 jp2")
	}
}

// TestRender_NormalizesISOCase 验证 ISO 大小写归一（ToUpper）—— 调用方喂 "jp"
// 与 "JP" 应映射到同一 country（去重键大写一致），且渲染输出用大写 ISO。
// 这一条让 frontrender 对调用方的 ISO 大小写不敏感，避免 "jp" 与 "JP" 渲染出两个
// 实际同国的 listener/provider/group（mihomo filter (?i)JP 对大小写本就不敏感，
// 但 listener name 是 case-sensitive 的，去重必须先归一）。
func TestRender_NormalizesISOCase(t *testing.T) {
	mixed := []CountrySpec{
		{ISO: "jp", Port: 7841, AuthUser: "u", AuthPass: "p"},   // 小写
		{ISO: "JP", Port: 9999, AuthUser: "u2", AuthPass: "p2"}, // 大写重复，必须被丢弃
	}
	out, err := Render(mixed, constProviderURL, constControllerSecret)
	if err != nil {
		t.Fatalf("Render 返回错误：%v", err)
	}
	if bytes.Contains(out, []byte("9999")) {
		t.Error("ISO 大小写归一去重未生效（大写重复条目的 port 9999 出现了）")
	}
	// 渲染输出的 entry name 必须用大写 mixed-JP（归一后渲染），不是 mixed-jp。
	if !bytes.Contains(out, []byte("mixed-JP")) {
		t.Error("渲染输出应用大写归一后的 ISO（mixed-JP），实际未出现")
	}
	if bytes.Contains(out, []byte("mixed-jp")) {
		t.Error("渲染输出现小写 mixed-jp —— ISO 未被 ToUpper 归一")
	}
}

// TestRender_OutputIsParseableYAML 验证渲染输出是 mihomo 可解析的 YAML（而非"看起来像
// YAML 的文本"）。这一条用 yaml.v3 解析整份输出，断言无错。它是 frontrender"纯文本渲染"
// 与"mihomo 真的能 Parse"之间的桥——如果模板缩进/转义错（如把 secret 嵌进含特殊字符
// 的位置），yaml.Unmarshal 会当场报错，比子串断言更早暴露形态错误。
//
// 注意这是 frontrender 唯一对 YAML 库的依赖路径，且只在测试里——产品代码零 YAML 库依赖
// （纯 text/template 渲染），与 #1 acceptance criteria"frontrender 零 mihomo 依赖"并列
// 的另一条"frontrender 是纯渲染"的红线在这里以测试形态守住。
func TestRender_OutputIsParseableYAML(t *testing.T) {
	yamlBytes := renderOK(t)
	var node yaml.Node
	if err := yaml.Unmarshal(yamlBytes, &node); err != nil {
		t.Fatalf("渲染输出不是合法 YAML：%v", err)
	}
}

// --- P3 (#7) external-ui 段 ------------------------------------------------
// P3 把 metacubexd 面板经 //go:embed 嵌入二进制，启动时解压到 mihomo homeDir/ui/ 再喂
// external-ui 路径。frontrender 这一层负责把 external-ui 段渲染进 YAML，并焊死两条
// mihomo 自动下载触发口（防"2025-06 GitHub 黑名单运行时拉取"红线，
// 见 VENDORING.md 与交接文件安全红线段）：
//
//   - external-ui-url: ""   钉死空字面。mihomo DefaultRawConfig() 把 external-ui-url 默认
//     填成 metacubexd gh-pages.zip URL；不显式渲染会被默认填回，AutoDownloadUI 拿到非空
//     URL 即"随时会下载覆盖 embed 资源"状态。空字面是擦掉默认的唯一手段。
//   - external-ui-name: ""  钉死空字面。NewUiUpdater 见非空 name 会把 serve 路径改成
//     ui/<name>，与 embed 解压点 homeDir/ui 错位 → 面板 404。
//
// 默认（无 WithExternalUIPath option）不渲染 UI 段——保护未启用 P3 时老输出形态不变
// （不污染 17 个旧测的断言语境）。

// constExternalUIPath 是 P3 测试用的固定 external-ui 路径替身。生产里它是 mihomo homeDir
// 的 ui/ 子目录（filepath.Join(homeDir,"ui")），frontrender 只原样嵌进 YAML 不处理合法性。
const constExternalUIPath = "/tmp/warp-go-mihomo-home/ui"

// renderWithUI 是 P3 切片共享的 happy path：喂 7 国 + 固定 URL + secret + WithExternalUIPath，
// 断言无错并返回 YAML。它把 P3 option 调用收敛到一处，让各切片专注断言自己关心的那行。
func renderWithUI(t *testing.T) []byte {
	t.Helper()
	out, err := Render(sevenCountries(), constProviderURL, constControllerSecret, WithExternalUIPath(constExternalUIPath))
	if err != nil {
		t.Fatalf("Render 带 WithExternalUIPath 返回错误：%v", err)
	}
	if len(out) == 0 {
		t.Fatal("Render 带 WithExternalUIPath 返回空 YAML")
	}
	return out
}

// TestRender_ExternalUISectionPresent 验证喂 WithExternalUIPath 后输出含 external-ui 行
// 且行尾含传入路径。这是 P3 option 生效的基线断言。
func TestRender_ExternalUISectionPresent(t *testing.T) {
	yaml := renderWithUI(t)
	contains(t, yaml, "external-ui:", "external-ui 段标识")
	contains(t, yaml, constExternalUIPath, "external-ui 路径嵌入 YAML")
}

// TestRender_ExternalUIURLAlwaysEmpty 验证 external-ui-url 显式渲染成空字面 ""。
// 这是陷阱 #3 的防线——擦掉 mihomo DefaultRawConfig 的 gh-pages.zip 默认，防 AutoDownloadUI
// 触发运行时拉取（#7 acceptance"避开 2025-06 GitHub 黑名单风险"）。
func TestRender_ExternalUIURLAlwaysEmpty(t *testing.T) {
	yaml := renderWithUI(t)
	contains(t, yaml, "external-ui-url:", "external-ui-url 字段标识（显式渲染）")
	line := findLineWith(yaml, "external-ui-url:")
	line = strings.TrimSpace(line)
	val := strings.TrimSpace(strings.TrimPrefix(line, "external-ui-url:"))
	// 值应为空字面（"" 或 ''）—— 视模板 用哪种引号；剥引号后必须空。
	val = strings.Trim(val, "\"'")
	if val != "" {
		t.Errorf("external-ui-url 应为空字面，got %q（mihomo DefaultRawConfig 默认未被擦干净 → 黑名单拉取风险）", val)
	}
}

// TestRender_ExternalUINameEmpty 验证 external-ui-name 显式渲染成空字面 ""。
// 这是陷阱 #4 的防线——防 NewUiUpdater 见非空 name 把 serve 路径改成 ui/<name> 与 embed
// 解压点错位（面板 404）。
func TestRender_ExternalUINameEmpty(t *testing.T) {
	yaml := renderWithUI(t)
	contains(t, yaml, "external-ui-name:", "external-ui-name 字段标识（显式渲染）")
	line := findLineWith(yaml, "external-ui-name:")
	line = strings.TrimSpace(line)
	val := strings.TrimSpace(strings.TrimPrefix(line, "external-ui-name:"))
	val = strings.Trim(val, "\"'")
	if val != "" {
		t.Errorf("external-ui-name 应为空字面，got %q（非空会让 mihomo serve 路径错位 → 面板 404）", val)
	}
}

// TestRender_NoExternalUISectionByDefault 验证默认（无 WithExternalUIPath）不渲染 external-ui
// 段。这条保护未启用 P3 的老部署：他们的输出 YAML 形态不变，不会因 P3 落地而多出未知段。
// 也是回归守门——与全部 17 个旧测的"renderOK 不带 option"语境对齐。
func TestRender_NoExternalUISectionByDefault(t *testing.T) {
	yaml := renderOK(t)
	notContains(t, yaml, "external-ui:", "默认不渲染 external-ui 段（未启用 P3）")
	notContains(t, yaml, "external-ui-url:", "默认不渲染 external-ui-url 段（未启用 P3）")
	notContains(t, yaml, "external-ui-name:", "默认不渲染 external-ui-name 段（未启用 P3）")
}

// itoa 是为让本测试文件不依赖 strconv 包、避免在断言里反复 strconv.Itoa 的简写。
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [12]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

// findLineWith 返回 yaml 中含 key 前缀的第一整行（已 trim）。用于行语义断言（secret 长度）。
func findLineWith(yaml []byte, key string) string {
	for _, line := range strings.Split(string(yaml), "\n") {
		if strings.Contains(line, key) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// entryBlock 返回 yaml 中 anchor 所在 entry 起到下一个 sibling entry 起点止的内容。
// anchor 是 entry 唯一标识（如 "mixed-JP"、"provider-JP"、"group-JP"、
// "external-controller-cors:"），按它切分各国段不会串段。
//
// 切分规则：测出 anchor 行的缩进与 entry 类型（列表项 "- " / map 键 "key:" / 顶级段），
// 然后从该行扫到下一个"同缩进 + 同类型 entry"的行即止。这是 listeners 列表项
// （"  - name: mixed-KR" 把 "mixed-JP" 截断地关键）、proxy-providers map 键
// （"  provider-KR:" 把 "provider-JP" 截断）、proxy-groups 列表项、顶级 scalar
// （"rules:" 把整段 proxy-groups 截断）四种渲染形态的统一切法。
func entryBlock(yaml []byte, anchor string) (string, bool) {
	s := string(yaml)
	if !strings.Contains(s, anchor) {
		return "", false
	}
	lines := strings.Split(s, "\n")
	// 找首行含 anchor 的行号 —— entry name 是 entry 唯一标识，首中即本 entry 起点。
	anchorLine := -1
	for i, line := range lines {
		if strings.Contains(line, anchor) {
			anchorLine = i
			break
		}
	}
	if anchorLine < 0 {
		return "", false
	}
	// anchor 行缩进 + entry 类型。entry 类型由行首（去掉缩进后）首个有意义 token 决定：
	//   - "- "          → 列表项
	//   - "key:"        → map 键
	//   - 其它（如注释、scalar）→ 顶级 scalar 段，直接按"下一同缩进任意项"截。
	indent := indentOf(lines[anchorLine])
	anchorType := entryType(lines[anchorLine])
	if anchorType == "" {
		return lines[anchorLine], true // 单行 scalar，无 sibling
	}
	// 从 anchor 行扫到下一个 sibling（同缩进 + 同类型）。
	var out []string
	for i := anchorLine; i < len(lines); i++ {
		line := lines[i]
		if i == anchorLine {
			out = append(out, line)
			continue
		}
		// 空行 / 注释行：段内噪声，照收。
		if line == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			out = append(out, line)
			continue
		}
		// 同缩进 + 同 entry 类型 → 下一个 sibling，截断。
		if indentOf(line) == indent && entryType(line) == anchorType {
			break
		}
		// 行首非空白（缩进=0）且不是 anchor 类型 → 顶级段，截断（如 rules: 截 proxy-groups）。
		if indentOf(line) == 0 {
			break
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n"), true
}

// indentOf 返回行前导空格数（制表符按 1 计，frontrender 模板仅用空格）。
func indentOf(line string) int {
	n := 0
	for _, r := range line {
		if r == ' ' || r == '\t' {
			n++
		} else {
			break
		}
	}
	return n
}

// entryType 返回行的 entry 类型："list"（列表项 "- "）、"map"（map 键 "key:" ）、
// "scalar"（顶级 scalar 如 "rules:"），或 "" 空行/注释/不可分类。
func entryType(line string) string {
	trimmed := strings.TrimLeft(line, " \t")
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return ""
	}
	if strings.HasPrefix(trimmed, "- ") || trimmed == "-" {
		return "list"
	}
	// map 键：含 ":" 且冒号前是单个 token（无空格、无引号）。简单判：含 ":" 即算 map。
	if strings.Contains(trimmed, ":") {
		return "map"
	}
	return "scalar"
}

// portBindsLoopback 判 yaml 中含 port 的行前后窗口内是否含 127.0.0.1。用于 mixed inbound
// "7840 绑回环"的近邻断言（覆盖拆分 listen+port 与单行 addr 两种形态）。
func portBindsLoopback(yaml []byte, port int) bool {
	lines := strings.Split(string(yaml), "\n")
	portStr := itoa(port)
	for i, line := range lines {
		if !strings.Contains(line, portStr) {
			continue
		}
		// 取前后 2 行窗口，看是否含 127.0.0.1。
		lo := i - 2
		if lo < 0 {
			lo = 0
		}
		hi := i + 2
		if hi >= len(lines) {
			hi = len(lines) - 1
		}
		for j := lo; j <= hi; j++ {
			if strings.Contains(lines[j], "127.0.0.1") {
				return true
			}
		}
	}
	return false
}

// hasPrivateCIDR 判 seg 是否含至少一个私网/回环 CIDR（10/8、172.16/12、192.168/16、
// 127/8 或 fc00::/7 for IPv6 ULA）。用于 skip-auth-prefixes"限私网"的断言。
func hasPrivateCIDR(seg string) bool {
	privates := []string{
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "127.0.0.0/8",
		"fc00::/7", "::1/128",
	}
	for _, p := range privates {
		if strings.Contains(seg, p) {
			return true
		}
	}
	return false
}

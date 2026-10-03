package route

import (
	"errors"
	"io"
	"io/fs"
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// silenceLogs 屏蔽测试期间的日志输出（引擎/下载路径会打大量 log）。
func silenceLogs(t *testing.T) {
	t.Helper()
	orig := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(orig) })
}

func TestParseRulesValid(t *testing.T) {
	text := `
# 注释行
proxy,geosite:google
proxy, geosite:geolocation-!cn
direct,geoip:private
direct,geoip:lan
direct,geoip:CN
direct,domain:example.com

  proxy,domain:foo.com  
`
	rules, err := ParseRules(text)
	if err != nil {
		t.Fatalf("ParseRules 意外失败：%v", err)
	}
	want := []Rule{
		{Action: "proxy", Kind: "geosite", Value: "google"},
		{Action: "proxy", Kind: "geosite", Value: "geolocation-!cn"},
		{Action: "direct", Kind: "geoip", Value: "private"},
		{Action: "direct", Kind: "geoip", Value: "lan"},
		{Action: "direct", Kind: "geoip", Value: "CN"},
		{Action: "direct", Kind: "domain", Value: "example.com"},
		{Action: "proxy", Kind: "domain", Value: "foo.com"},
	}
	if len(rules) != len(want) {
		t.Fatalf("规则数 = %d，期望 %d：%+v", len(rules), len(want), rules)
	}
	for i := range want {
		if rules[i] != want[i] {
			t.Errorf("第 %d 条规则 = %+v，期望 %+v", i, rules[i], want[i])
		}
	}
}

func TestParseRulesEmpty(t *testing.T) {
	rules, err := ParseRules("")
	if err != nil {
		t.Fatalf("空文本应解析成功：%v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("空文本应得到 0 条规则，得到 %d", len(rules))
	}
	rules, err = ParseRules("# 只有注释\n\n  \n")
	if err != nil || len(rules) != 0 {
		t.Fatalf("纯注释/空行应解析为 0 条规则（err=%v）", err)
	}
}

func TestParseRulesMalformed(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		wantLine int
		wantSub  string // 错误信息应包含的子串
	}{
		{"缺逗号", "proxy,geosite:google\ndirect\n", 2, "缺少逗号"},
		{"行为非法", "proxy,geosite:google\nbanana,geosite:cn\n", 2, "行为"},
		{"条件无冒号", "proxy,geosite:google\nproxy,geositeX\n", 2, "缺少 `:`"},
		{"类型非法", "proxy,geosite:google\nproxy,weird:cn\n", 2, "条件类型"},
		{"值空", "proxy,geosite:\n", 1, "条件值不能为空"},
		{"空行报行号", "\n\nproxy,geosite:cn\nbad\n", 4, "第 4 行"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseRules(tc.text)
			if err == nil {
				t.Fatalf("期望解析失败")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("错误信息 %q 缺少子串 %q", err.Error(), tc.wantSub)
			}
			// 行号校验：错误信息应提到第 wantLine 行
			if tc.wantLine > 0 && !strings.Contains(err.Error(), "第 "+strconv.Itoa(tc.wantLine)+" 行") {
				t.Errorf("错误信息 %q 应指向第 %d 行", err.Error(), tc.wantLine)
			}
		})
	}
}

func TestParseRulesReject(t *testing.T) {
	text := `
REJECT,geosite:category-ads-all
reject,geosite:category-ads-all
Reject,GeoSite:Category-Ads
REJECT,domain:doubleclick.net
direct,geoip:cn
`
	rules, err := ParseRules(text)
	if err != nil {
		t.Fatalf("ParseRules 应接受 reject 行为：%v", err)
	}
	want := []Rule{
		{Action: "reject", Kind: "geosite", Value: "category-ads-all"},
		{Action: "reject", Kind: "geosite", Value: "category-ads-all"},
		{Action: "reject", Kind: "geosite", Value: "Category-Ads"},
		{Action: "reject", Kind: "domain", Value: "doubleclick.net"},
		{Action: "direct", Kind: "geoip", Value: "cn"},
	}
	if len(rules) != len(want) {
		t.Fatalf("规则数 = %d，期望 %d：%+v", len(rules), len(want), rules)
	}
	for i := range want {
		if rules[i] != want[i] {
			t.Errorf("第 %d 条 = %+v，期望 %+v（action/kind 应归一化为小写，domain 值保留原样）", i, rules[i], want[i])
		}
	}
}

// TestDomainValueCasePreserved 验证 domain 条件值在解析期保持原样（大小写
// 不归一化），运行时匹配才做大小写折叠 —— 用户要求"规则行为条件不区分
// 大小写，域名条件除外"。
func TestDomainValueCasePreserved(t *testing.T) {
	rules, err := ParseRules("proxy,DOMAIN:Example.COM\n")
	if err != nil {
		t.Fatalf("大写 DOMAIN 类型应解析成功：%v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("期望 1 条规则，得到 %d", len(rules))
	}
	if rules[0].Kind != "domain" {
		t.Errorf("kind 应归一化为小写 domain，得到 %q", rules[0].Kind)
	}
	if rules[0].Value != "Example.COM" {
		t.Errorf("domain 值应保留原样，得到 %q（不应归一化）", rules[0].Value)
	}
}

func TestDefaultRulesParses(t *testing.T) {
	rules, err := ParseRules(DefaultRules)
	if err != nil {
		t.Fatalf("默认模板必须可解析：%v", err)
	}
	if len(rules) != 10 {
		t.Fatalf("默认模板应有 10 条规则，得到 %d：%+v", len(rules), rules)
	}
	want := []Rule{
		{Action: "reject", Kind: "geosite", Value: "category-ads-all"},
		{Action: "direct", Kind: "geosite", Value: "private"},
		{Action: "direct", Kind: "geoip", Value: "private"},
		{Action: "proxy", Kind: "geosite", Value: "google"},
		{Action: "proxy", Kind: "geoip", Value: "google"},
		{Action: "proxy", Kind: "geosite", Value: "geolocation-!cn"},
		{Action: "proxy", Kind: "geoip", Value: "telegram"},
		{Action: "proxy", Kind: "ip-cidr", Value: "1.1.1.1/32", Prefix: netip.MustParsePrefix("1.1.1.1/32")},
		{Action: "direct", Kind: "geosite", Value: "cn"},
		{Action: "direct", Kind: "geoip", Value: "cn"},
	}
	for i := range want {
		if rules[i] != want[i] {
			t.Errorf("第 %d 条 = %+v，期望 %+v", i, rules[i], want[i])
		}
	}
}

func TestEnsureRulesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.txt")

	created, err := EnsureRulesFile(path)
	if err != nil {
		t.Fatalf("EnsureRulesFile 失败：%v", err)
	}
	if !created {
		t.Fatal("文件缺失时 EnsureRulesFile 应返回 created=true")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取新文件失败：%v", err)
	}
	if string(data) != DefaultRules {
		t.Errorf("新文件内容应为默认模板，实际：\n%s", data)
	}

	// 已存在时不覆盖
	custom := "proxy,domain:custom.example\n"
	if err := os.WriteFile(path, []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	created, err = EnsureRulesFile(path)
	if err != nil || created {
		t.Fatalf("已存在时 EnsureRulesFile 应返回 (false, nil)，得到 (%v, %v)", created, err)
	}
	data, _ = os.ReadFile(path)
	if string(data) != custom {
		t.Errorf("已有文件被覆盖：%q", data)
	}
}

func TestLoadRulesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.txt")
	os.WriteFile(path, []byte("proxy,geosite:google\n# c\ndirect,geoip:cn\n"), 0o644)

	rules, err := LoadRulesFile(path)
	if err != nil {
		t.Fatalf("LoadRulesFile 失败：%v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("期望 2 条规则，得到 %d", len(rules))
	}

	_, err = LoadRulesFile(filepath.Join(dir, "missing.txt"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("缺失文件应返回 fs.ErrNotExist，得到 %v", err)
	}
}

func TestWatchRulesFile(t *testing.T) {
	silenceLogs(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.txt")
	os.WriteFile(path, []byte("proxy,domain:foo.com\n"), 0o644)

	reloaded := make(chan []Rule, 4)
	stop, err := watchRulesFile(path, 20*time.Millisecond, func(rules []Rule, err error) {
		if err != nil {
			reloaded <- nil
			return
		}
		reloaded <- rules
	})
	if err != nil {
		t.Fatalf("watchRulesFile 启动失败：%v", err)
	}
	defer stop()

	// 内容变更 → 检测到并回调
	time.Sleep(30 * time.Millisecond) // 越过首个 tick，确认基线不误报
	select {
	case <-reloaded:
		t.Fatal("基线快照不应触发回调")
	default:
	}

	os.WriteFile(path, []byte("direct,domain:foo.com\n"), 0o644)
	select {
	case rules := <-reloaded:
		if len(rules) != 1 || rules[0].Action != "direct" {
			t.Errorf("回调规则 = %+v，期望 [direct,domain:foo.com]", rules)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("文件变更后 3s 内未触发热重载回调")
	}

	// 文件被改为非法内容 → 回调收到 nil + 错误
	os.WriteFile(path, []byte("garbage line\n"), 0o644)
	select {
	case rules := <-reloaded:
		if rules != nil {
			t.Errorf("非法内容应回调 nil 规则，得到 %+v", rules)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("非法内容变更后 3s 内未触发回调")
	}

	// stop 后不再回调
	stop()
	os.WriteFile(path, []byte("proxy,domain:bar.com\n"), 0o644)
	select {
	case <-reloaded:
		t.Fatal("stop 后不应再触发回调")
	case <-time.After(80 * time.Millisecond):
	}
}

// TestParseRulesDefault 验证 `default:<action>` 兜底声明行的解析（东哥要求：
// 规则文件显式声明未命中兜底，优先于代码硬编码，代码仅作无声明时回退）。
func TestParseRulesDefault(t *testing.T) {
	rules, err := ParseRules("proxy,geosite:google\ndefault: direct\n")
	if err != nil {
		t.Fatalf("ParseRules 应接受 default 行：%v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("规则数 = %d，期望 2（1 条规则 + 1 条 default）：%+v", len(rules), rules)
	}
	if rules[1] != (Rule{Action: "direct", Kind: "default", Value: ""}) {
		t.Errorf("default 行应解析为 {direct,default,}，得到 %+v", rules[1])
	}

	// 大小写不敏感 + 冒号后空格容忍 + reject 也可作兜底。
	rules, err = ParseRules("DEFAULT: REJECT\n")
	if err != nil {
		t.Fatalf("ParseRules 应接受大写 DEFAULT + 空格：%v", err)
	}
	if len(rules) != 1 || rules[0].Action != "reject" || rules[0].Kind != "default" {
		t.Errorf("default 大小写/空格处理错误：%+v", rules)
	}

	// 重复声明报错。
	if _, err := ParseRules("default:direct\ndefault:proxy\n"); err == nil {
		t.Fatal("default 重复声明应报错")
	}

	// 非法行为报错。
	if _, err := ParseRules("default:banana\n"); err == nil {
		t.Fatal("default 非法行为应报错")
	}
}

// TestParseRulesIPCIDR 锁定 ip-cidr 条件：现网曾因裸写 `proxy,1.1.1.1/32`
// （无前缀）触发解析错误 + 启动崩溃循环（CT103 NRestarts=6，40000 无监听），
// 也曾因缺少该条件类型而无法表达「1.1.1.1 走隧道」。
func TestParseRulesIPCIDR(t *testing.T) {
	rules, err := ParseRules("proxy,ip-cidr:1.1.1.1/32\ndirect,ip-cidr:10.0.0.0/8\n")
	if err != nil {
		t.Fatalf("ip-cidr 规则应可解析：%v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("应解析出 2 条规则，得到 %d", len(rules))
	}
	if got := rules[0].Prefix.String(); got != "1.1.1.1/32" {
		t.Errorf("第 1 条 Prefix = %q，期望 1.1.1.1/32", got)
	}
	if got := rules[1].Prefix.String(); got != "10.0.0.0/8" {
		t.Errorf("第 2 条 Prefix = %q，期望 10.0.0.0/8", got)
	}

	// 非法值必须在解析期就报错（拖到匹配期会让脏规则静默不生效）。
	for _, bad := range []string{"proxy,ip-cidr:1.1.1.1", "proxy,ip-cidr:not-an-ip", "proxy,ip-cidr:"} {
		if _, err := ParseRules(bad + "\n"); err == nil {
			t.Errorf("非法 ip-cidr 行 %q 应报错", bad)
		}
	}
}

// TestDefaultRulesParseAndEngineInit 锁定默认模板可被解析且引擎能初始化——
// 模板里新增的条件类型若拼错，启动路径会直接失败。
func TestDefaultRulesParseAndEngineInit(t *testing.T) {
	if _, err := ParseRules(DefaultRules); err != nil {
		t.Fatalf("默认规则模板应可解析：%v", err)
	}
	e := newTestEngine(t, DefaultRules)

	// 1.1.1.1 必须走隧道，且不能被后面的 direct,geoip:cn 抢走。
	act, _, matched := e.Match("1.1.1.1", netip.Addr{})
	if !matched || act != ActionProxy {
		t.Errorf("1.1.1.1 应命中 proxy,ip-cidr:1.1.1.1/32，得到 (%s, matched=%v)", act, matched)
	}
	// 同网段外不得命中 /32 规则：matched2 必须取本次结果（复用旧 matched 会让
	// 断言静默失效——审查 t_794f439e minor），并区分「命中错规则」与「未命中」。
	act2, _, matched2 := e.Match("1.0.0.1", netip.Addr{})
	if matched2 {
		t.Errorf("1.0.0.1 应完全未命中（模板无匹配规则），得到 act=%s matched=%v——若命中需区分是 ip-cidr 越界还是其他规则误配", act2, matched2)
	}
	_ = act // act 已在上方断言使用，保留变量避免改动前段
}

// Package frontrender renders the mihomo YAML the B-1 front proxy runs on, as
// pure text/template output. It is deliberately a pure renderer with no mihomo
// dependency: the seam contract is "fixed countries + port table + provider URL
// + controller secret → YAML bytes", so P1-C can run and be unit-tested in
// parallel with P1-B without a real mihomo compiled in (see #1 spec, Testing
// Decisions, "frontrender 独立缝").
//
// What it renders, and why each bit is the way it is:
//
//   - listener 段: one per-country mixed inbound per ISO country, each bound to
//     127.0.0.1:<port> (JP=7841…TH=7847), plus the mihomo mixed inbound on
//     127.0.0.1:7840 and external-controller on 127.0.0.1:9090. Loopback-only
//     binding is the "裸口不落公网, 对外由前置 TLS+auth" contract (#1 user story 17/18).
//   - proxy-providers 段: the vpngate-meridian gh-pages mihomo_tested_openvpn.yaml
//     fetched as a remote HTTP provider, once per country, filtered by ISO
//     country code so one upstream YAML serves all 7 countries (#1 user story 19).
//     Every provider carries an override that forces udp:false on every node,
//     so warp-go's SOCKS UDP ASSOCIATE cannot bypass the tunnel and leak the
//     real address (#1 user story 23, safety red line).
//   - proxy-groups 段: one select group per country, each `use`-ing its own
//     country-filtered provider — the "一份 provider YAML 供 7 国复用" form (#1
//     user story 16/19).
//   - security fields: external-controller carries a 32-char random secret,
//     precise CORS (never the wildcard `*`), and a firewall; each per-country
//     listener carries its own auth + skip-auth-prefixes limited to private
//     ranges, so a compromise of one country's port cannot cascade to the other
//     six (#1 user story 21/22).
//
// frontrender never reads reg.json, never writes secrets to disk, and never
// imports any mihomo internal package. The controller secret is passed in by
// the caller (generated at process start, never committed).
package frontrender

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
)

// CountrySpec is one row of the B-1 nation→port table the caller feeds Render.
// ISO is the ISO 3166-1 alpha-2 country code (JP/KR/US/VN/RU/ID/TH for P1-C);
// it doubles as the provider filter key and the selector-group name. Port is the
// per-country listener port on 127.0.0.1 (7841..7847). AuthUser/AuthPass are the
// per-country listener's own credentials — independent per country so one
// country's breach cannot reach the other six (#1 user story 22). Empty
// AuthUser means "no auth" and is not a supported production posture here; the
// caller is expected to supply per-country creds (run-time generated or read
// from a non-committed source).
type CountrySpec struct {
	ISO      string // ISO 3166-1 alpha-2 国家码，兼 provider filter key 与 selector 名
	Port     int    // per-country listener 绑回环端口（7841..7847）
	AuthUser string // 该国 listener 独立认证用户名；空表示该 listener 由前置 TLS 承担认证
	AuthPass string // 该国 listener 独立认证口令
}

// privateCIDRs is the set of private/loopback prefixes per-country listeners
// skip-auth for. Limiting skip-auth to these (rather than all of 0.0.0.0/0)
// is the "skip-auth-prefixes 限私网" half of #1 user story 22; the other half
// (independent per-country auth) is the per-listener users field.
var privateCIDRs = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"127.0.0.0/8",
	"fc00::/7", // IPv6 ULA
	"::1/128",  // IPv6 loopback
}

// corsAllowOrigins is the precise CORS allow-origins list for
// external-controller. It intentionally excludes the wildcard `*` (#1 user
// story 21 red line): any honest deploy should pin its own dashboard origin
// here. We default to explicit localhost origins so the render output is a
// valid, non-wildcard CORS config out of the box; a deploy overrides it by
// post-processing the rendered YAML (frontrender is a pure renderer and does
// not know the deploy's dashboard origin).
var corsAllowOrigins = []string{
	"http://127.0.0.1:9090",
	"http://localhost:9090",
}

// The controllerSecret passed to Render is embedded verbatim into the secret
// scalar field — frontrender never generates or persists it. Render refuses an
// empty or sub-32-char secret: a weak secret here is a safety red line, not a
// soft preference (#1 user story 21).
const minSecretLen = 32

// renderModel is the shape text/template renders. It collects every field the
// template references so the template has a single, complete data root, and so
// adding a field forces an explicit model change rather than a stray template
// action reaching into something unmodeled.
//
// P3 (#7) fields: ExternalUIPath / ExternalUIURL / ExternalUIName render the
// mihomo external-ui 段. ExternalUIPath is set only when WithExternalUIPath is
// passed (guard 条件 {{if .ExternalUIPath}} 控制整段渲染与否，未启用 P3 时整段不渲染，
// 老输出形态不变)。ExternalUIURL 与 ExternalUIName 恒为空字面 "" —— 这是焊死 mihomo
// DefaultRawConfig 默认（gh-pages.zip URL）与 NewUiUpdater 路径错位（非空 name）两条
// 自动下载触发口的防线（见 render_test.go P3 切片注释 + VENDORING.md）。
type renderModel struct {
	Countries        []CountrySpec
	ProviderURL      string
	ControllerSecret string
	PrivateCIDRs     []string
	CorsAllowOrigins []string
	ExternalUIPath   string // P3：external-ui 磁盘路径；空 = 未启用 P3，整段不渲染
	ExternalUIURL    string // P3：恒空字面，擦掉 mihomo gh-pages.zip 默认
	ExternalUIName   string // P3：恒空字面，防 NewUiUpdater serve 路径错位
}

// yamlTemplate is the single source of truth for the rendered output shape.
// Keeping it as one text/template (rather than string concatenation) lets the
// "render three sections" contract be read top-to-bottom and lets a future
// reviewer diff one form, not chase fragments. Indentation is mihomo YAML's
// two-space convention. The template never interpolates secrets into a position
// that would make them a YAML key or a comment; the secret lands in the secret:
// scalar field only.
const yamlTemplate = `# Rendered by warp-go frontproxy/frontrender (P1-C, #3).
# 7 国 per-country listener + mihomo mixed inbound + external-controller +
# remote HTTP proxy-providers (vpngate-meridian gh-pages) filtered by ISO +
# one selector group per country. 裸口全绑回环，对外由前置 TLS+auth 暴露。
# 所有 provider 节点显式 udp:false（warp-go SOCKS UDP ASSOCIATE 绕过隧道会泄真实地址）。

# ---- inbound ----------------------------------------------------------
# mihomo mixed inbound：同时接受 http+socks，绑回环与各国 listener 对齐（裸口不落公网）。
mixed-port: 7840
bind-address: 127.0.0.1

# per-country listeners：每国一个 mixed inbound，各绑回环自己的端口（JP=7841..TH=7847），
# 各自独立 auth + skip-auth-prefixes 限私网（一国被攻破不连累其余六国）。
listeners:
{{- range .Countries}}
  - name: mixed-{{.ISO}}
    type: mixed
    listen: 127.0.0.1
    port: {{.Port}}
    # udp:false on the inbound too：该国 listener 入口不接 SOCKS UDP ASSOCIATE，
    # 与所有 provider 节点 udp:false 共同堵住绕过隧道的真实地址泄漏。
    udp: false
{{- if .AuthUser}}
    users:
      - username: {{.AuthUser}}
        password: {{.AuthPass}}
{{- end}}
    skip-auth-prefixes:
{{- range $.PrivateCIDRs}}
      - {{.}}
{{- end}}
{{- end}}

# ---- controller -------------------------------------------------------
# external-controller 绑回环 9090；强 32 字符 random secret + 精确 CORS（never ` + "`*`" + `）+ firewall。
external-controller: 127.0.0.1:9090
secret: {{.ControllerSecret}}
external-controller-cors:
  allow-origins:
{{- range .CorsAllowOrigins}}
    - {{.}}
{{- end}}
  allow-private-network: false
{{- if .ExternalUIPath}}
# external-controller-ui（P3 #7）：metacubexd 面板经 //go:embed 嵌入二进制，启动时
# 由 frontui.Extract 解压到此磁盘路径（mihomo homeDir/ui/），mihomo 用 http.FileServer
# 从该目录 serve。external-ui-url 与 external-ui-name 显式空字面焊死两条自动下载触发口：
# external-ui-url 空 擦掉 mihomo DefaultRawConfig 的 gh-pages.zip 默认（防 2025-06 黑名单
#   运行时拉取覆盖 embed 资源——#7 acceptance）；
# external-ui-name 空 防 NewUiUpdater 把 serve 路径改成 ui/<name> 与解压点错位（面板 404）。
external-ui: {{.ExternalUIPath}}
external-ui-url: ""
external-ui-name: ""
{{- end}}
# firewall：controller 段白名单源段，与 secret/CORS 三件套并立；默认 deny-all ingress。
firewall:
  deny-all-ingress: true

# ---- providers --------------------------------------------------------
# 一份 vpngate-meridian gh-pages 的 mihomo_tested_openvpn.yaml，按 ISO 国家码 filter
# 切分给 7 国 selector 组（一份 provider YAML 供 7 国复用）。每个 provider 节点经
# override 强制 udp:false。
proxy-providers:
{{- range .Countries}}
  provider-{{.ISO}}:
    type: http
    url: {{$.ProviderURL}}
    # 落盘缓存路径（.gitignore 已忽略 mihomo/proxy_providers/，缓存不入仓）。
    path: ./mihomo/proxy_providers/provider-{{.ISO}}.yaml
    interval: 3600
    # 按 ISO 国家码 filter 切分：节点名/国家字段含该国码即归该国 provider。
    filter: "(?i){{.ISO}}"
    override:
      # 所有 provider 节点显式 udp:false（#1 user story 23 红线）。
      udp: false
    health-check:
      enable: true
      url: https://www.gstatic.com/generate_204
      interval: 300
      timeout: 5000
      lazy: true
      expected-status: 204
{{- end}}

# ---- groups -----------------------------------------------------------
# 每国一个 select group，use 自己的 country-filtered provider。
proxy-groups:
{{- range .Countries}}
  - name: group-{{.ISO}}
    type: select
    use:
      - provider-{{.ISO}}
{{- end}}

# rules：默认全走 DIRECT，frontrender 不负责路由策略（由后续票 P1-B/P1-D 接）。
rules:
  - MATCH,DIRECT
`

// Render renders the mihomo YAML for the given countries, remote HTTP provider
// URL, and external-controller secret. It is the single entry point of
// frontrender and the seam the unit tests drive (see render_test.go: "喂固定
// countries + port 表 + providerURL + controllerSecret，断言输出 YAML 含预期
// listener/provider/group 段 + 全部安全字段").
//
// Render is pure: same inputs → same bytes, no I/O, no mihomo import. It returns
// an error rather than panicking on bad input so the caller (main.go wiring,
// a later slice) can surface it; today the only failure modes are a too-weak
// controller secret or a template execution failure (the latter is a programmer
// bug, not a run-time condition, but we still surface it rather than panic).
// Option configures a Render call's optional behavior (functional-option
// pattern). It keeps Render's positional arg list stable so the P1-C contract
// (countries + providerURL + controllerSecret) is not broken when P3 (#7) needs
// to inject the external-ui path. Render accepts opts as a variadic, so every
// existing call site `Render(a, b, c)` compiles unchanged (zero opts = default
// behavior = no external-ui segment rendered).
type Option func(*renderModel)

// WithExternalUIPath enables P3 (#7): render an `external-ui:` segment pointing
// at the disk path the mihomo kernel serves metacubexd from. It also pins
// `external-ui-url: ""` and `external-ui-name: ""` (see renderModel doc) to weld
// shut mihomo's two auto-download triggers — the 2025-06 GitHub-blacklist
// runtime fetch (DefaultRawConfig defaults external-ui-url to a gh-pages.zip
// URL) and the NewUiUpdater path-mismatch when external-ui-name is non-empty.
//
// path is embedded verbatim; frontrender does not validate it is a mihomo
// IsSafePath (that is the caller / mihomo-side wiring's job, see
// frontproxy/mihomo Engine WithEmbeddedUI). Empty path is a no-op (the option
// is dropped) so passing WithExternalUIPath("") does not spuriously render a UI
// segment.
func WithExternalUIPath(path string) Option {
	return func(m *renderModel) {
		if path != "" {
			m.ExternalUIPath = path
		}
	}
}

func Render(countries []CountrySpec, providerURL string, controllerSecret string, opts ...Option) ([]byte, error) {
	if len(controllerSecret) < minSecretLen {
		return nil, fmt.Errorf("frontrender：controller secret 长度 %d < %d（弱 secret，#1 user story 21 红线）",
			len(controllerSecret), minSecretLen)
	}
	if providerURL == "" {
		return nil, fmt.Errorf("frontrender：providerURL 为空（远程 HTTP provider 源不可缺）")
	}
	if len(countries) == 0 {
		return nil, fmt.Errorf("frontrender：countries 为空")
	}
	// 去重 + 顺序稳定：同一 ISO 出现两次只渲染一次，顺序保留首次出现位。
	seen := make(map[string]bool, len(countries))
	uniq := make([]CountrySpec, 0, len(countries))
	for _, c := range countries {
		iso := strings.ToUpper(strings.TrimSpace(c.ISO))
		if iso == "" {
			return nil, fmt.Errorf("frontrender：country ISO 为空")
		}
		if c.Port <= 0 || c.Port > 65535 {
			return nil, fmt.Errorf("frontrender：country %s port %d 非 1..65535", iso, c.Port)
		}
		if seen[iso] {
			continue
		}
		seen[iso] = true
		c.ISO = iso
		uniq = append(uniq, c)
	}

	tmpl, err := template.New("frontrender").Parse(yamlTemplate)
	if err != nil {
		// yamlTemplate is a package constant; a parse error is a programmer bug.
		return nil, fmt.Errorf("frontrender：模板解析失败：%w", err)
	}
	// 构造 base model：countries/providerURL/secret 走位置参数，P3 三字段零值
	// （ExternalUIPath 空 = 不渲染 external-ui 段；URL/Name 恒空仅在 WithExternalUIPath
	// 启用整段后由模板字面 "" 落地）。
	model := renderModel{
		Countries:        uniq,
		ProviderURL:      providerURL,
		ControllerSecret: controllerSecret,
		PrivateCIDRs:     privateCIDRs,
		CorsAllowOrigins: corsAllowOrigins,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&model)
		}
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, model); err != nil {
		return nil, fmt.Errorf("frontrender：模板执行失败：%w", err)
	}
	return buf.Bytes(), nil
}

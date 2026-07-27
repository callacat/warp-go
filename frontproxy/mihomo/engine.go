package mihomo

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"path/filepath"
	"sync"

	"github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/hub"
	"github.com/metacubex/mihomo/hub/executor"
	"github.com/metacubex/mihomo/hub/route"

	"warp/frontproxy/frontui"
)

// engine.go is the B-1 code-layer integration gate (#4): it hosts mihomo via
// hub.Parse so the library-mode kernel can boot and shut down cleanly inside
// warp-go's single binary, WITHOUT reaching a real vpngate node. Real-node
// routing (gVisor netstack → OpenVPN → WARP edge IP) is B-1-PoC-2's job; P1-B
// only proves the kernel start/stop round trip and that the anti-corruption
// seam (OvpnEdgeResolver in resolver.go) is the only surface the rest of
// warp-go ever touches.
//
// Safety red lines honored here, verbatim from #1 spec:
//  - NEVER reassign net.DefaultResolver. mihomo's binary main.go does
//    (PreferGo=true + a custom Dial). The library path (hub.Parse →
//    executor.ApplyConfig) does not, and neither does this Engine. warp-go uses
//    DefaultResolver for edge resolution; clobbering it would break the core
//    path. The tests pin this invariant via the defaultResolverSnapshot helpers
//    at the bottom of this file.
//  - No real proxy/provider node is ever configured. The config NewEngine
//    accepts must be caller-supplied so a future B-1-PoC-2 caller can pass the
//    frontrender output; P1-B itself only feeds the minimal kernel-boot config
//    from the test. Empty config is refused (ErrEmptyConfig) so no Engine boots
//    a kernel with nothing to route on.

// ErrEmptyConfig is returned by NewEngine when the config carries no payload.
// The minimal P1-B kernel-boot config sets log level + a loopback mixed inbound
// on an ephemeral port; anything shorter than that has no route surface and is
// refused rather than silently booting an inert kernel.
var ErrEmptyConfig = errors.New("mihomo engine: 配置为空")

// Engine hosts mihomo's library-mode kernel inside warp-go. It is the only
// object that ever parses mihomo config or drives hub.Parse; all mihomo internal
// imports stay confined to this package (the anti-corruption收口, #1 user story 3).
//
// An Engine is single-use: NewEngine parses (without applying), Start applies it
// once via hub.Parse (boots the kernel), Close shuts it down via executor.Shutdown
// and records that the Engine is no longer usable. Start after Close is an error;
// the recovery position is to build a fresh Engine.
type Engine struct {
	cfg     []byte
	homeDir string
	// embeddedUI 是可选的 fs.FS（通常是 frontui.DistFS 即 //go:embed 进来的
	// metacubexd 静态产物）。Start 在 hub.Parse 前把它解压到 homeDir/ui/ 并把
	// 该路径喂给 hub.WithExternalUI，同时 route.SetEmbedMode(true) 关 mutate 路由。
	// 零值 fs.FS（nil）= 未启用 P3 → Start 不碰 UI（opt-in，见 P3 切片 D 测）。
	embeddedUI fs.FS

	// options is the list of hub.Option applied at Start (e.g. WithSecret on the
	// frontrender config). It is kept rather than applied at NewEngine because
	// hub.Parse consumes both in one call.
	options []hub.Option

	// appliedOnce guards against a double Start on the same Engine — hub.Parse
	// holds a package-level mutex and re-entry through it is a state leak, not a
	// refresh. Callers that need a re-parse build a new Engine.
	appliedOnce bool

	mu      sync.Mutex
	started bool
	closed  bool
}

// Option configures an Engine at construction. The functional-option pattern
// matches frontrender's call shape and keeps NewEngine's positional arg list
// short (config + home are mandatory, the rest are optional).
type Option func(*Engine)

// WithHomeDir sets the directory mihomo writes its side artifacts to (geodata
// cache, downloaded providers in later tickets). It MUST point at a process-
// owned, the repo-uncontaminated directory: mihomo resolves provider cache and
// geodata paths relative to its home dir, and writing into the repo tree would
// both pollute the working copy and risk committing those artifacts (#1 user
// story 24 — provider 落盘缓存不进版本控制).
func WithHomeDir(dir string) Option {
	return func(e *Engine) {
		if dir != "" {
			e.homeDir = dir
		}
	}
}

// WithHubOption attaches a mihomo hub.Option (e.g. hub.WithSecret,
// hub.WithExternalController) that Start applies alongside the parsed config.
// This is the seam a future B-1-PoC-2 or full-B-1 wiring uses to inject the
// external-controller secret + per-country listener auth produced at runtime —
// never committed, never read from reg.json.
func WithHubOption(opt hub.Option) Option {
	return func(e *Engine) {
		if opt != nil {
			e.options = append(e.options, opt)
		}
	}
}

// WithEmbeddedUI enables P3 (#7): the Engine will, at Start, extract the given
// fs.FS's metacubexd subtree into homeDir/ui/ and wire mihomo to serve it from
// there as external-controller-ui. efs is typically frontui.DistFS (the
// //go:embed all:assets/metacubexd FS); passing nil is a no-op so the option can
// be conditionally appended without a caller-side nil guard.
//
// The three-step wiring (Extract → hub.WithExternalUI → route.SetEmbedMode) runs
// in Start, NOT here: it must happen after homeDir is fixed (constant.SetHomeDir)
// and before hub.Parse (so config resolution sees external-ui pointing at the
// real disk path, and so the router reads embedMode=true when it is built). See
// the homeDir/ui extraction site in Start and frontui.Extract's contract for the
// safety-path reasoning (homeDir/ui is a homeDir subtree by construction → passes
// mihomo IsSafePath).
func WithEmbeddedUI(efs fs.FS) Option {
	return func(e *Engine) {
		if efs != nil {
			e.embeddedUI = efs
		}
	}
}

// homeDirDefault returns a safe-ish default home dir when WithHomeDir is not
// given. P1-B callers always pass a temp dir in tests; production wiring must
// pass one too. A relative path is resolved absolute so mihomo's path resolver
// behaves deterministically regardless of the process's cwd at the time of
// Start.
func homeDirDefault() string {
	abs, err := filepath.Abs(".")
	if err != nil {
		return "."
	}
	return filepath.Join(abs, ".mihomo-home")
}

// NewEngine parses the mihomo YAML carried by cfg and prepares the Engine for
// Start. It does NOT boot the kernel — that deferred to Start so a constructed
// Engine can be inspected (HomeDir, etc.) before the side-effectful ApplyConfig
// call that listener binds and DNS start. Empty cfg is refused (ErrEmptyConfig).
func NewEngine(cfg []byte, opts ...Option) (*Engine, error) {
	if len(cfg) == 0 {
		return nil, ErrEmptyConfig
	}
	e := &Engine{cfg: append([]byte(nil), cfg...), homeDir: homeDirDefault()}
	for _, opt := range opts {
		if opt != nil {
			opt(e)
		}
	}
	return e, nil
}

// HomeDir returns the directory mihomo writes side artifacts into. It is the
// absolute path NewEngine resolved (from WithHomeDir or the default), and the
// value passed to constant.SetHomeDir at Start.
func (e *Engine) HomeDir() string { return e.homeDir }

// Start boots the mihomo kernel via hub.Parse. It:
//   - pins net.DefaultResolver is left untouched by recording it is the caller's
//     expectation (the assertion policy lives in the test; the Engine simply
//     never reassigns it),
//   - points mihomo's home dir at the Engine's homeDir so writes stay out of the
//     repo tree,
//   - applies cfg + any WithHubOption opts through hub.Parse exactly once.
//
// Start is NOT safe to call twice on the same Engine: hub.Parse acquires a
// package-level executor mutex and re-entering it is a state leak. A caller
// needing a re-parse builds a fresh Engine.
func (e *Engine) Start() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return errors.New("mihomo engine: 已 Close，不再可用；如需重新解析请新建 Engine")
	}
	if e.appliedOnce {
		e.mu.Unlock()
		return errors.New("mihomo engine: 已 Start，重复 Start 会泄漏内核状态；如需重解析请新建 Engine")
	}
	e.appliedOnce = true
	e.started = true
	cfg := e.cfg
	homeDir := e.homeDir
	hubOpts := append([]hub.Option(nil), e.options...)
	embeddedUI := e.embeddedUI
	e.mu.Unlock()

	// Point mihomo at our home dir before any path-relative artifact is written.
	// SetHomeDir is the documented entry; must run before hub.Parse so config
	// resolution and geodata use it.
	constant.SetHomeDir(homeDir)

	// P3 (#7) embedded UI wiring — runs only when WithEmbeddedUI was passed.
	// Order is a hard constraint: each step must complete before hub.Parse
	// below, because hub.Parse → executor.ApplyConfig builds mihomo's router
	// and listeners, which read both external-ui (for the /ui file server) and
	// route.embedMode (to gate /restart /configs /rules /upgrade mutate routes).
	//   1. frontui.Extract: write the embed.FS subtree to homeDir/ui/. dest is a
	//      homeDir child by construction, so it passes mihomo constant.IsSafePath
	//      (constant/path.go:88 — path must be under homeDir). Extract is itself
	//      idempotent (skips when homeDir/ui/index.html already exists & non-empty),
	//      so a repeated Start-of-a-fresh-Engine against the same homeDir does not
	//      rewrite 155 files. frontui has zero mihomo import; dependency arrow is
	//      mihomo → frontui (one-way, like frontrender before it).
	//   2. hub.WithExternalUI(homeDir/ui): tell mihomo where the disk UI lives, so
	//      http.FileServer(http.Dir(uiPath)) serves the extracted files at /ui.
	//   3. route.SetEmbedMode(true): switch the /api surface to read-only (no
	//      restart/configs/rules/upgrade). This is the single-binary embed-mode
	//      contract — the binary's UI never comes from a runtime fetch, so neither
	//      should its control surface accept mutation that assumes one.
	//
	// external-ui-url and external-ui-name are NOT set here: frontrender pins them
	// to "" in the rendered YAML (DefaultRawConfig otherwise defaults
	// external-ui-url to a metacubexd gh-pages.zip URL → a runtime fetch that
	// both defeats embed and risks the 2025-06 GitHub blacklist).
	if embeddedUI != nil {
		uiPath := filepath.Join(homeDir, "ui")
		if err := frontui.Extract(embeddedUI, "assets/metacubexd", uiPath); err != nil {
			return fmt.Errorf("mihomo engine: 解压嵌入 UI 到 %s 失败：%w", uiPath, err)
		}
		hubOpts = append(hubOpts, hub.WithExternalUI(uiPath))
		route.SetEmbedMode(true)
	}

	// hub.Parse parses the YAML and applies the config into mihomo's globals
	// (listeners, DNS, rules, providers). It is the library-mode boot the
	// spec's P1-B point ("Engine 走 hub.Parse 最小骨架") names directly.
	if err := hub.Parse(cfg, hubOpts...); err != nil {
		return err
	}
	return nil
}

// Close shuts the mihomo kernel down via executor.Shutdown and marks the Engine
// no longer usable. It is idempotent: a second Close is a no-op. It does NOT
// restore net.DefaultResolver — because the Engine never touched it; leaving
// it as-is is the correct revert.
func (e *Engine) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	wasStarted := e.started
	e.mu.Unlock()

	if wasStarted {
		executor.Shutdown()
	}
	return nil
}

// --- defaultResolver invariants ------------------------------------------------
// These read-only helpers live in the non-test file so they compile into the
// package binary too: they document, in code the reviewer can grep for, that the
// Engine's contract forbids reassigning net.DefaultResolver. The Engine
// implementation itself never calls them; they exist so the test asserting the
// invariant has a single source of truth for "what would count as a mutation".
// Reading DefaultResolver is not a mutation and not a red-line violation — only
// writes (PreferGo = ...; Dial = ...) are.

// netDefaultResolverPreferGo reports the current PreferGo flag of net.DefaultResolver.
// It is read-only: the Engine never sets it. The test uses this as the snapshot
// value to assert Start left it unchanged.
func netDefaultResolverPreferGo() bool {
	return net.DefaultResolver.PreferGo
}

// netDefaultResolverDialIsSet reports whether net.DefaultResolver.Dial has been
// replaced from its default (nil). mihomo's binary main.go sets it to a custom
// dialer; the library Engine MUST NOT, so "was it set" is the bit the test pins.
// Detection compares against the zero-value package-level *Resolver by reading
// the Dial field: the stdlib default is a nil Dial.
func netDefaultResolverDialIsSet() bool {
	return net.DefaultResolver.Dial != nil
}

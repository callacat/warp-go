package mihomo

import (
	"os"
	"path/filepath"
	"testing"

	"warp/frontproxy/frontui"
)

// embed_ui_test.go 是 P3 (#7) 切片 D 的红测，焊住 Engine 经 WithEmbeddedUI 把
// //go:embed 的 metacubexd 静态产物在 Start 时解压到 mihomo homeDir/ui/、并把该路径
// 喂给 hub.WithExternalUI 的端到端链路。
//
// 这条链路是 P3 的运行时收口：frontui.DistFS（embed）→ frontui.Extract（解压到磁盘）
// → hub.WithExternalUI(homeDir/ui)（喂 external-ui 路径）→ route.SetEmbedMode(true)
// （关 mutate 路由，只读面板语义）。前两步在 frontui 包单测已验，这里验"Engine 三步走
// 在 hub.Parse 前正确组合"。
//
// 安全红线（与 engine_test.go 同守）：
//   - net.DefaultResolver 不被改写。frontui 零 mihomo import + 纯文件 I/O，不碰 resolver；
//     但 Engine.Add 的 route.SetEmbedMode + hub.WithExternalUI 也不应破红线。本测同样
//     快照 DefaultResolver 前后断言不变。
//   - home 用 t.TempDir() 沙盒，untracked。
//
// 与 engine_test 的资源共享：本包测试串行跑（不 t.Parallel），hub.Parse 持 executor
// 互斥锁，无并发竞争。最小 config 沿用 engineRoundTripConfig（mixed-port 0 + 回环 + silent）。
//
// D2（验 SetEmbedMode 真关掉 /restart /configs-patch 等 mutate 路由）评估后留为未做项：
// 它需起 9090 HTTP server + secret + PATCH 请求，与 engine_test 端口/资源模型互斥风险高，
// 超出 P3 "把嵌入链路焊住"的范畴，留作后继 ticket。

// TestEngine_WithEmbeddedUI_ExtractsToHomeDir_UI 验证带 WithEmbeddedUI 的 Engine 在
// Start 后把 embed 资源解压到 homeDir/ui/，且面板入口 index.html 落地。这是 P3 整条
// embed→Extract→homeDir 链路的端到端锚。
//
// 契约松绑到"含 index.html 即可过"：vendoring 后 frontui.DistFS 含真产物 155 文件，
// 占位态仅 index.html + .gitkeep。两种态下 homeDir/ui/index.html 都应存在（Extract 的
// 成功不变量由 frontui 包单测已焊住，这里只验 Engine 真调了 Extract 且路径拼对）。
func TestEngine_WithEmbeddedUI_ExtractsToHomeDir_UI(t *testing.T) {
	dir := t.TempDir()

	// 安全红线快照：DefaultResolver 在 Start 前后必须不变（与 engine_test 同守）。
	wantPrefer, wantDialSet := defaultResolverSnapshot()

	eng, err := NewEngine(engineRoundTripConfig(),
		WithHomeDir(dir),
		WithEmbeddedUI(frontui.DistFS),
	)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	if eng == nil {
		t.Fatal("NewEngine 返回 nil Engine")
	}

	// HomeDir 应是传入的 t.TempDir（WithEmbeddedUI 不改 homeDir）。
	if got := eng.HomeDir(); got != dir {
		t.Fatalf("Engine.HomeDir = %q, want %q（WithEmbeddedUI 不应改 homeDir）", got, dir)
	}

	if err := eng.Start(); err != nil {
		t.Fatalf("Engine.Start： %v（frontui.DistFS extract 或 hub.Parse 失败——是否未跑 scripts/vendor-metacubexd.sh？）", err)
	}
	defer eng.Close()

	// 安全红线：Start 后 DefaultResolver 仍不变。
	if gotPrefer, gotDialSet := defaultResolverSnapshot(); gotPrefer != wantPrefer || gotDialSet != wantDialSet {
		t.Fatalf("net.DefaultResolver 被 Engine.Start 改写：PreferGo before=%v after=%v, Dial set before=%v after=%v",
			wantPrefer, gotPrefer, wantDialSet, gotDialSet)
	}

	// 核心断言：embed 资源已解压到 homeDir/ui/，面板入口 index.html 落地。
	uiIndex := filepath.Join(dir, "ui", "index.html")
	if info, err := os.Stat(uiIndex); err != nil || info.IsDir() || info.Size() == 0 {
		t.Errorf("Start 后 %s 不存在/是目录/空（Engine 未把 embed 资源解压到 homeDir/ui/）", uiIndex)
	}

	// _nuxt/ 子树应一并解出（端到端验 all: 前缀链路，不止落 index.html）。
	// 占位态此断言红 —— 但 index.html 已落地，证 Engine 三步走基本通；_nuxt 缺只提示未 vendor。
	nuxtDir := filepath.Join(dir, "ui", "_nuxt")
	if entries, err := os.ReadDir(nuxtDir); err != nil || len(entries) == 0 {
		t.Errorf("Start 后 %s 子树缺失或空（未 vendor 或 frontui Embed all: 前缀失效）", nuxtDir)
	}
}

// TestEngine_WithoutEmbeddedUI_NoUIDir 验证不带 WithEmbeddedUI 的 Engine（默认）Start 后
// homeDir 下不应出现 ui/ 目录——P3 是 opt-in，未启用时不该有副作用目录残留。这是
// "WithEmbeddedUI 是 opt-in 而非默认"的回归守门。
func TestEngine_WithoutEmbeddedUI_NoUIDir(t *testing.T) {
	dir := t.TempDir()

	eng, err := NewEngine(engineRoundTripConfig(), WithHomeDir(dir))
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	if err := eng.Start(); err != nil {
		t.Fatalf("Engine.Start: %v", err)
	}
	defer eng.Close()

	// 默认（无 WithEmbeddedUI）不该解压出 ui/ 目录。
	if _, err := os.Stat(filepath.Join(dir, "ui")); !os.IsNotExist(err) {
		t.Errorf("默认（无 WithEmbeddedUI）Start 后 homeDir/ui 不该存在（P3 应 opt-in，无副作用）err=%v", err)
	}
}

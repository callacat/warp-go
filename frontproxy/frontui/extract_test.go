package frontui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// extract_test.go 是 P3 切片 B 的红测，焊住 Extract 的"embed.FS → 磁盘目录"契约。
// Extract 是 P3 运行时链路的枢纽：mihomo 的 external-ui 接磁盘路径（不是 fs.FS），
// 所以启动时要把 //go:embed 的资源解压到 mihomo homeDir/ui/，再把该绝对路径喂给
// external-ui + hub.WithExternalUI。Extract 就是那步解压。
//
// 测试用 fstest.MapFS 造迷你虚拟文件系统（不依赖真 metacubexd 产物、零 vendoring 依赖），
// t.TempDir 沙盒磁盘 dest。每条切片焊 Extract 状态机的一个分支：
//
//   - 空目录 → 全量解压（落地 index.html + 子树）
//   - dest/index.html 已存在且非空 → 幂等 skip（Engine 可重复 Start 不浪费 IO）
//   - dest 非空但无 index.html → 中断残留，清空重解（防半截目录令面板缺文件）
//   - src 本身缺 index.html → 返错（embed 源损坏的硬失败）
//
// seam 形态与 frontrender/render_test.go 一致：固定输入 + 行为断言 + helper 收口。

// miniFS 构造一个含 index.html + 一个 _nuxt/ 子文件 + 一个 _fonts/ 子文件的迷你 fs，
// 覆盖"顶层条目 + 两个下划线子目录"的真实结构形态（与真 metacubexd 同形，只体量小）。
// sub 是 Extract 的子树参数——这里用 "assets/metacubexd" 与生产签名对齐，让测试桩与真
// embed 路径同构。
func miniFS() fstest.MapFS {
	return fstest.MapFS{
		"assets/metacubexd/index.html":     {Data: []byte("<html><body>panel</body></html>")},
		"assets/metacubexd/_nuxt/entry.js": {Data: []byte("// entry js")},
		"assets/metacubexd/_fonts/a.woff2": {Data: []byte{0x1f, 0x1b, 0x00}},
		"assets/metacubexd/config.js":      {Data: []byte("export default {}")},
	}
}

// testSub 与生产 Extract 调用的 sub 参数同值（"assets/metacubexd"），让测试桩与真 embed
// 路径同构——若 embed 路径漂移，测试与生产会同步显形。
const testSub = "assets/metacubexd"

// fileExists 断言磁盘路径存在、非目录、且非空。
func fileExists(t *testing.T, path string) bool {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Size() > 0
}

// assertExtracted 熔断式断言 dest 下 index.html / _nuxt/entry.js / _fonts/a.woff2 /
// config.js 全部落地且非空——验 Extract 把 src 子树完整拍平到 dest 根（去掉了 sub 前缀）。
func assertExtracted(t *testing.T, dest string) {
	t.Helper()
	for _, rel := range []string{"index.html", "_nuxt/entry.js", "_fonts/a.woff2", "config.js"} {
		p := filepath.Join(dest, rel)
		if !fileExists(t, p) {
			t.Errorf("Extract 后 dest 缺 %s（应落地且非空）", rel)
		}
	}
}

// TestExtract_DropsIndexHtml 验证把迷你 FS 的 sub 子树解压到空 dest，落地全部文件。
// 这是 Extract 的基线契约：全量拍平 src/<sub>/* 到 dest/*。
func TestExtract_DropsIndexHtml(t *testing.T) {
	dest := t.TempDir()
	if err := Extract(miniFS(), testSub, dest); err != nil {
		t.Fatalf("Extract 返回错误：%v", err)
	}
	assertExtracted(t, dest)
}

// TestExtract_IdempotentIfIndexExists 验证幂等：dest/index.html 已存在且非空时第二次
// Extract 直接 return nil 不重写文件。Engine 可重复 Start 而不每次全量 IO 真产物的 155 文件。
// 检测手段：先全量解一次，给 index.html 打个标记字节 → 再 Extract → 断言标记还在（未重写）。
func TestExtract_IdempotentIfIndexExists(t *testing.T) {
	dest := t.TempDir()
	if err := Extract(miniFS(), testSub, dest); err != nil {
		t.Fatalf("首次 Extract 失败：%v", err)
	}
	// 给 index.html 写一个 sentinel，第二次幂等 Extract 不应覆盖它。
	indexPath := filepath.Join(dest, "index.html")
	sentinel := []byte("SENTINEL_AFTER_FIRST_EXTRACT")
	if err := os.WriteFile(indexPath, sentinel, 0o644); err != nil {
		t.Fatalf("写 sentinel 失败：%v", err)
	}
	if err := Extract(miniFS(), testSub, dest); err != nil {
		t.Fatalf("第二次（幂等）Extract 返回错误：%v", err)
	}
	got, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("读 index.html 失败：%v", err)
	}
	if string(got) != string(sentinel) {
		t.Errorf("幂等 Extract 不该重写 index.html：got %q want %q", got, sentinel)
	}
}

// TestExtract_ReclaimsPartialState 验证中断残留自愈：dest 非空但 **缺 index.html**
// （模拟上次 Extract 写到一半中断），再 Extract 应清空 dest 重解，落地完整文件。
// 没有这一条，中断残留会让面板缺文件而 Extract 静默 skip。
func TestExtract_ReclaimsPartialState(t *testing.T) {
	dest := t.TempDir()
	// 制造中断残留：dest 里有半个 _nuxt/ 子文件，但没有 index.html。
	if err := os.MkdirAll(filepath.Join(dest, "_nuxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "_nuxt", "partial.js"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Extract(miniFS(), testSub, dest); err != nil {
		t.Fatalf("Extract 残留态返错：%v", err)
	}
	assertExtracted(t, dest)
	// 残留的 partial.js 不该还在（残留整个被清掉重解）。
	if _, err := os.Stat(filepath.Join(dest, "_nuxt", "partial.js")); !os.IsNotExist(err) {
		t.Errorf("残留 _nuxt/partial.js 未被清空重解（err=%v）", err)
	}
}

// TestExtract_RejectsIfIndexMissingAfterExtract 验证 src 子树本身缺 index.html 时
// Extract 返错而不是静默落地一个无入口的面板目录。这是 embed 源损坏的硬失败防线。
func TestExtract_RejectsIfIndexMissingAfterExtract(t *testing.T) {
	// 构造一个有 _nuxt/ 但无 index.html 的损坏 FS。
	broken := fstest.MapFS{
		"assets/metacubexd/_nuxt/entry.js": {Data: []byte("// only js")},
	}
	dest := t.TempDir()
	err := Extract(broken, testSub, dest)
	if err == nil {
		t.Fatal("src 缺 index.html 时 Extract 应返错，实际 nil")
	}
	// 错误信息应点明 index.html 缺失（便于上层诊断）。
	if !strings.Contains(err.Error(), "index.html") {
		t.Errorf("错误信息应点明 index.html 缺失，got：%v", err)
	}
}

// TestExtract_RealDistFSToDisk 是切片 B 的端到端锚：用真 frontui.DistFS（不是 MapFS 桩）
// 跑一遍 Extract，验 //go:embed 的资源真能落盘且面板入口齐。它是切片 A（embed 可读性）与切片 B
// （Extract 契约）的串接——把"打进二进制"与"解到磁盘"两道闸焊在一起。
//
// 双态价值：占位态跑，DistFS 仅 2 文件，`_nuxt/` 不存在 → 红（与 TestDistFS_FileCountMeetsFloor
// 同因）；vendor 后跑，155 文件全解 → 绿。配上 TestDistFS_*，未 vendor 状态有两条独立红线暴露。
func TestExtract_RealDistFSToDisk(t *testing.T) {
	dest := t.TempDir()
	if err := Extract(DistFS, "assets/metacubexd", dest); err != nil {
		t.Fatalf("Extract 真 DistFS 返错：%v（是否未跑 scripts/vendor-metacubexd.sh？）", err)
	}
	// 面板入口必须落地 —— 这是"解压真成功"的最小证明。
	if !fileExists(t, filepath.Join(dest, "index.html")) {
		t.Error("Extract 真 DistFS 后 dest 缺 index.html")
	}
	// _nuxt/ bundle 子树应被一并解出（验 all: 前缀链路端到端生效，不止落 index.html）。
	// 占位态此断言红 —— 与 TestDistFS_HasNuxtBundle 同因（被 //go:embed 默认丢弃或未 vendor）。
	nuxtDir := filepath.Join(dest, "_nuxt")
	if entries, err := os.ReadDir(nuxtDir); err != nil || len(entries) == 0 {
		t.Error("Extract 真 DistFS 后 dest/_nuxt/ 子树缺失或空（未 vendor 或 all: 前缀失效）")
	}
}

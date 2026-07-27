package frontui

import (
	"io/fs"
	"testing"
)

// embed_test.go 是 P3 切片 A 的红测，焊住 //go:embed all:assets/metacubexd 真把
// metacubexd 静态产物打进了二进制。这是 P3 最隐蔽的陷阱的防线：Go 的 //go:embed
// 默认跳过 `_` / `.` 开头的路径，metacubexd 顶层恰是 `_nuxt/`（109 JS/CSS）与
// `_fonts/`（32 woff2）。漏写 `all:` 前缀会让这两棵子树整体消失——首页 200、所有
// 资源 404、面板白屏。文件数 ≥ 100 的断言把"漏 all:"变成 CI 可见的红，而不是"index.html
// 在就够了"的静默绿。
//
// 测试 seam 与 frontrender/render_test.go 一致（见该文件头注释）：固定输入 + 行为断言
// + helper 收口，每条切片焊一个可观察行为。这里"输入"是编译期 //go:embed 的结果 DistFS，
// "行为"是它的可达文件集合。

// assetRoot 是 DistFS 内 metacubexd 产物子树的根路径。//go:embed all:assets/metacubexd
// 保留目录前缀（embed.FS 的条目路径含 assets/metacubexd/ 段，不拍平到根），所以面板入口
// 实际在 assets/metacubexd/index.html、bundle 在 assets/metacubexd/_nuxt/。这与 Extract
// 的 sub="assets/metacubexd" 参数同源——sub 指向这个子树根，Extract 把它拍平到磁盘 dest。
const assetRoot = "assets/metacubexd"

// minAssetFiles 是"真实 metacubexd 产物已 vendored"的下界。真实版 v1.270.5 的 release
// tarball 解包后实测 155 文件（根 14 + _nuxt/ 109 JS+CSS + _fonts/ 32 woff2；gh-pages tree
// 同版 API 报 157，多出的 2 是 GitHub Pages 专属的 .nojekyll + CNAME，不在 release tarball，
// P3 以 release tarball 为源故落地 155）。占位态仅 index.html + .gitkeep = 2 文件，远低于
// 此阈值 → 红，提示跑 scripts/vendor-metacubexd.sh。取 100 而非 155 给上游升级留余量
// （metacubexd bundle 数会随版本浮动），只要 ≥100 就证明 _nuxt/ 真被打进来。
const minAssetFiles = 100

// hasDir 判断 DistFS 中是否存在名为 name 的子目录（相对 assetRoot）。用于 _nuxt/
// / _fonts/ 这类"必须存在却被 //go:embed 默认丢弃"的子树的断言。
func hasDir(t *testing.T, name string) bool {
	t.Helper()
	info, err := fs.Stat(DistFS, assetRoot+"/"+name)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// countFiles 统计 DistFS 中 assetRoot 子树下所有非目录条目数（含 _nuxt/ 与 _fonts/
// 子树）。走 fs.WalkDir 全遍历，覆盖 _nuxt/ 与 _fonts/ 这类被 all: 前缀拉进来的子树——
// 若 all: 漏写，这两棵子树 WalkDir 根本不会出现，计数值会掉到个位数。
func countFiles(t *testing.T) int {
	t.Helper()
	n := 0
	err := fs.WalkDir(DistFS, assetRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		n++
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir DistFS@%s 失败：%v（assets/metacubexd 目录不存在或 //go:embed 失败）",
			assetRoot, err)
	}
	return n
}

// TestDistFS_HasIndexHtml 断言面板入口 index.html 在 assetRoot 根。这是 //go:embed
// 指令合法（目录存在、有匹配文件）的最小证明——占位态此测也绿（占位 index.html 在），
// 它不是"真产物在"的证明，只是"embed 没整体崩"的下界。
func TestDistFS_HasIndexHtml(t *testing.T) {
	if _, err := fs.Stat(DistFS, assetRoot+"/index.html"); err != nil {
		t.Fatalf("DistFS 缺 %s/index.html：%v", assetRoot, err)
	}
}

// TestDistFS_HasNuxtBundle 断言 assetRoot/_nuxt/ 子目录存在且非空。_nuxt/ 是 metacubexd
// （Nuxt 静态导出）的 JS/CSS bundle 根，109 文件。它的名字以 `_` 开头——若 //go:embed
// 漏写 all:，这棵子树会被 Go 默认丢弃，hasDir("_nuxt") 返 false → 红。这是陷阱 #1 的
// 直接防线。
func TestDistFS_HasNuxtBundle(t *testing.T) {
	if !hasDir(t, "_nuxt") {
		t.Fatal("DistFS 缺 _nuxt/ 子目录 —— //go:embed 是否漏了 all: 前缀？（Go 默认丢弃 _ 开头的路径）")
	}
}

// TestDistFS_HasFontsDir 断言 assetRoot/_fonts/ 子目录存在。_fonts/ 含 32 个 woff2 字体，
// 同样以 `_` 开头，与 _nuxt/ 一道是漏 all: 前缀时的首批丢失项。
func TestDistFS_HasFontsDir(t *testing.T) {
	if !hasDir(t, "_fonts") {
		t.Fatal("DistFS 缺 _fonts/ 子目录 —— //go:embed 是否漏了 all: 前缀？（Go 默认丢弃 _ 开头的路径）")
	}
}

// TestDistFS_FileCountMeetsFloor 断言嵌入文件数 ≥ minAssetFiles（100）。这是 P3
// "真产物已 vendored" 的硬指标：v1.270.5 实测 155 文件，占位态仅 2 文件。低于阈值即红，
// 提示跑 scripts/vendor-metacubexd.sh。它也是"all: 前缀真生效"的间接证明——失 all:，
// _nuxt/+/fonts/ 整体消失，计数回落到 ~1，远低于 100。
func TestDistFS_FileCountMeetsFloor(t *testing.T) {
	n := countFiles(t)
	if n < minAssetFiles {
		t.Errorf("DistFS 仅 %d 个文件 < %d —— metacubexd 产物未 vendored（跑 scripts/vendor-metacubexd.sh）或 //go:embed 漏 all: 前缀",
			n, minAssetFiles)
	}
}

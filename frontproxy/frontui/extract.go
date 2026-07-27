package frontui

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// extract.go 提供 Extract —— P3 运行时链路把 //go:embed 的 metacubexd 静态产物拍平到
// 一个磁盘目录的解压器。
//
// 为什么需要解压而非直接喂 fs.FS：mihomo 的 external-controller-ui 接的是**磁盘路径**
// （string，见 mihomo config.go ExternalUI yaml:"external-ui" + hub/route/server.go 的
// http.FileServer(http.Dir(uiPath))）。它不认 fs.FS / embed.FS。所以 P3 的唯一路径是
// 启动时把 embed.FS 解压到 mihomo homeDir/ui/，再把该绝对路径喂给 external-ui +
// hub.WithExternalUI。Extract 就是这步解压。
//
// 为什么 Extract 是纯函数接 fs.FS 而非 embed.FS：让单元测试可用 fstest.MapFS 造迷你 FS
// 覆盖全部状态分支，零依赖 vendoring 真产物。embed.FS 零成本实现 fs.FS，生产路径传
// frontui.DistFS 即可（见 mihomo/engine.go 的 WithEmbeddedUI 接线）。
//
// dest 合法性（必须是 mihomo homeDir 子路径以过 IsSafePath）不是 Extract 的职责——
// Extract 只保证"dest 落地含 index.html"，dest 落点约束由 mihomo 包在喂
// hub.WithExternalUI(filepath.Join(homeDir,"ui")) 处满足。
//
// 状态机（三态 + 一硬失败）：
//   - dest 空 → 全量解压 src/<sub>/* 到 dest/*
//   - dest/index.html 已存在且非空 → 幂等 return nil（Engine 可重复 Start 不重写）
//   - dest 非空但缺 index.html → 中断残留，RemoveAll + 重建 + 全量重解
//   - src 子树本身缺 index.html → 返错（embed 源损坏硬失败，不静默落地无入口目录）

// Extract 把 src 文件系统里 sub 子树拍平到磁盘 dest 目录。
//
//   - src：embed.FS 或 fstest.MapFS（任何 fs.FS）。
//   - sub：src 内的子树根路径（如 "assets/metacubexd"）；空串解释为 src 根。
//   - dest：磁盘绝对目录；caller 保证它是 mihomo 可 serve 的安全路径（homeDir 子树）。
//
// 成功不变量：返回 nil 前 dest/index.html 必须存在且非空。任一中途写盘失败立即返错，
// 中断残留由下次调用的"缺 index.html → 清空重解"分支自愈。
func Extract(src fs.FS, sub, dest string) error {
	if src == nil {
		return fmt.Errorf("frontui.Extract：src 为 nil")
	}
	if dest == "" {
		return fmt.Errorf("frontui.Extract：dest 为空")
	}
	// 规整 sub：fs.FS 用正斜杠；空串或 "." 都解释为 src 根。
	sub = strings.TrimPrefix(sub, "./")
	root := sub
	if root == "" || root == "." {
		root = "."
	}

	// 幂等快速路径：dest/index.html 已存在且非空 → 直接 return nil。
	// 这是 Engine 可重复 Start 的性能前提（避免每次重写 155 文件）。
	idxPath := filepath.Join(dest, "index.html")
	if info, err := os.Stat(idxPath); err == nil && !info.IsDir() && info.Size() > 0 {
		return nil
	}

	// dest 既存且非空但缺 index.html → 视为上次解压中断的残留，清空重解。
	// 否则保持 dest（可能为空目录或不存在）并 MkdirAll。
	if entries, err := os.ReadDir(dest); err == nil && len(entries) > 0 {
		if err := os.RemoveAll(dest); err != nil {
			return fmt.Errorf("frontui.Extract：清空残留 dest 失败：%w", err)
		}
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return fmt.Errorf("frontui.Extract：创建 dest 失败：%w", err)
	}

	// 全量遍历 src/<root> 子树，把每个文件按相对路径写到 dest。
	// fs.WalkDir 的 path 是 fs.FS 规范的正斜杠形式；磁盘写要用 filepath.FromSlash 转 OS 分隔符。
	walkErr := fs.WalkDir(src, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// relPath = path 去掉 sub 前缀。sub=根("."") 时 fs.WalkDir 给的 path 不带前缀，
		// 直接用；否则 trim 掉 "sub/" 段。
		rel := path
		if root != "." {
			rel = strings.TrimPrefix(path, root+"/")
		}
		target := filepath.Join(dest, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("创建父目录 %s 失败：%w", filepath.Dir(target), err)
		}
		return writeFile(src, path, target)
	})
	if walkErr != nil {
		return fmt.Errorf("frontui.Extract：遍历/写盘失败：%w", walkErr)
	}

	// 成功不变量校验：dest/index.html 必须存在且非空。
	if info, err := os.Stat(idxPath); err != nil || info.IsDir() || info.Size() == 0 {
		return fmt.Errorf("frontui.Extract：解压后 dest 不含 index.html（src 子树 %q 可能损坏）", root)
	}
	return nil
}

// writeFile 把 src 中 path 处的文件写到磁盘 target。文件权限 0o644、目录 0o755 —— embed.FS
// 不保留 unix 权限，固定这套保守值（mihomo http.FileServer 仅需可读）。
func writeFile(src fs.FS, path, target string) error {
	r, err := src.Open(path)
	if err != nil {
		return fmt.Errorf("打开 src 文件 %s 失败：%w", path, err)
	}
	defer r.Close()
	w, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("创建 dest 文件 %s 失败：%w", target, err)
	}
	defer w.Close()
	if _, err := io.Copy(w, r); err != nil {
		return fmt.Errorf("复制 %s → %s 失败：%w", path, target, err)
	}
	return nil
}

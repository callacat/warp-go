//go:build !android

package main

import (
	"os"
	"path/filepath"
	"testing"

	"warp/core"
)

// TestDataDirEmptyOnDesktop 验证桌面端 dataDir() 返回空串：
// core.Options.DataDir 为空时保持执行目录锚定（桌面便携部署默认行为）——
// Android 沙箱修复在桌面端不得改变任何运行时路径。
func TestDataDirEmptyOnDesktop(t *testing.T) {
	if got := dataDir(); got != "" {
		t.Errorf("桌面端 dataDir() = %q，期望空串（保持执行目录锚定）", got)
	}
}

// TestServerInstanceWiresDataDir 验证 serverInstance() 创建 core.Server 时
// 传入 DataDir: dataDir()（桌面端为空串）且实例被缓存（幂等）。
//
// 注：core.Server.opts 是 core 包未导出字段，本包（main）无法直接断言
// opts.DataDir / opts.ConfigPath；此处断言可观察的服务契约（创建成功 +
// 幂等缓存），DataDir 锚定机制本身由 TestDataDirAnchorsConfigPath 覆盖。
func TestServerInstanceWiresDataDir(t *testing.T) {
	s := &Service{}
	srv, err := s.serverInstance()
	if err != nil {
		t.Fatalf("serverInstance() 返回错误：%v", err)
	}
	if srv == nil {
		t.Fatal("serverInstance() 返回 nil server")
	}
	again, err := s.serverInstance()
	if err != nil {
		t.Fatalf("第二次 serverInstance() 返回错误：%v", err)
	}
	if again != srv {
		t.Error("serverInstance() 未缓存实例：两次调用返回不同 server")
	}
}

// TestDataDirAnchorsConfigPath 验证 Android 修复依赖的 core 契约：
// DataDir 非空时 core.New 把相对 ConfigPath 解析为该目录下的绝对路径——
// SaveConfig 写入的 config.json 必须落在 DataDir 内（而非工作目录）。
func TestDataDirAnchorsConfigPath(t *testing.T) {
	dir := t.TempDir()
	srv := core.New(core.Options{ConfigPath: "config.json", DataDir: dir})
	if err := srv.SaveConfig(core.DefaultConfig()); err != nil {
		t.Fatalf("SaveConfig 返回错误：%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Errorf("config.json 未写入 DataDir=%s：%v", dir, err)
	}
}

// TestCachedDataDirEmptyOnDesktop 验证桌面端 cachedDataDir() 恒返回空串：
// GetStatus 的沙箱兜底检查在桌面端不触发（桌面无沙箱概念，零回归）。
func TestCachedDataDirEmptyOnDesktop(t *testing.T) {
	if got := cachedDataDir(); got != "" {
		t.Errorf("桌面端 cachedDataDir() = %q，期望空串", got)
	}
}

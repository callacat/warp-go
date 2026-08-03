package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNewResolvesPathsToExecDir 验证 resolveExecPath 把默认相对路径锚定到
// 可执行文件目录下的 config/ 子目录（GUI 双击启动时工作目录可能漂移，
// 文件必须跟着可执行文件走，并统一收拢进 config/）。
func TestNewResolvesPathsToExecDir(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable 不可用：%v", err)
	}
	wantDir := filepath.Join(filepath.Dir(exe), runtimeConfigDirName)

	s := New(Options{})
	if !filepath.IsAbs(s.opts.ConfigPath) {
		t.Errorf("ConfigPath 未锚定：%q", s.opts.ConfigPath)
	}
	if !strings.HasPrefix(s.opts.ConfigPath, wantDir) {
		t.Errorf("ConfigPath 不在 %s：%q", wantDir, s.opts.ConfigPath)
	}
	if !filepath.IsAbs(s.opts.StateFile) {
		t.Errorf("StateFile 未锚定：%q", s.opts.StateFile)
	}
	if !strings.HasPrefix(s.opts.StateFile, wantDir) {
		t.Errorf("StateFile 不在 %s：%q", wantDir, s.opts.StateFile)
	}
}

// TestNewKeepsAbsolutePaths 验证显式绝对路径不被改写。
func TestNewKeepsAbsolutePaths(t *testing.T) {
	s := New(Options{
		ConfigPath: "/tmp/warp-config.json",
		StateFile:  "/tmp/warp-reg.json",
	})
	if s.opts.ConfigPath != "/tmp/warp-config.json" {
		t.Errorf("绝对 ConfigPath 被改写：%q", s.opts.ConfigPath)
	}
	if s.opts.StateFile != "/tmp/warp-reg.json" {
		t.Errorf("绝对 StateFile 被改写：%q", s.opts.StateFile)
	}
}

// TestResolveExecPathEmpty 验证空串原样返回（不 panic）。
func TestResolveExecPathEmpty(t *testing.T) {
	if got := resolveExecPath(""); got != "" {
		t.Errorf("空串应原样返回，得到 %q", got)
	}
}

// TestNewResolvesToDataDir 验证 DataDir 非空时，相对路径锚定到 DataDir
// （Android 沙箱场景：可执行文件在 /system/bin 只读，运行时文件必须进沙箱）。
func TestNewResolvesToDataDir(t *testing.T) {
	s := New(Options{
		ConfigPath: "config.json",
		StateFile:  "reg.json",
		DataDir:    "/data/data/com.wails.app/files",
	})
	if s.opts.ConfigPath != "/data/data/com.wails.app/files/config.json" {
		t.Errorf("ConfigPath 未锚定到 DataDir：%q", s.opts.ConfigPath)
	}
	if s.opts.StateFile != "/data/data/com.wails.app/files/reg.json" {
		t.Errorf("StateFile 未锚定到 DataDir：%q", s.opts.StateFile)
	}
}

// TestNewDataDirKeepsAbsolute 验证 DataDir 存在时显式绝对路径仍不被改写。
func TestNewDataDirKeepsAbsolute(t *testing.T) {
	s := New(Options{
		ConfigPath: "/tmp/warp-config.json",
		StateFile:  "/tmp/warp-reg.json",
		DataDir:    "/data/data/com.wails.app/files",
	})
	if s.opts.ConfigPath != "/tmp/warp-config.json" {
		t.Errorf("绝对 ConfigPath 被改写：%q", s.opts.ConfigPath)
	}
	if s.opts.StateFile != "/tmp/warp-reg.json" {
		t.Errorf("绝对 StateFile 被改写：%q", s.opts.StateFile)
	}
}

// TestNewEmptyDataDirStillExecAnchors 验证 DataDir 为空时行为与默认一致
// （桌面/CLI 回归锁：仍锚定到可执行文件目录下的 config/ 子目录）。
func TestNewEmptyDataDirStillExecAnchors(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable 不可用：%v", err)
	}
	wantDir := filepath.Join(filepath.Dir(exe), runtimeConfigDirName)
	s := New(Options{})
	if !strings.HasPrefix(s.opts.ConfigPath, wantDir) {
		t.Errorf("空 DataDir 下 ConfigPath 未走默认锚定：%q（期望前缀 %q）", s.opts.ConfigPath, wantDir)
	}
	if !strings.HasPrefix(s.opts.StateFile, wantDir) {
		t.Errorf("空 DataDir 下 StateFile 未走默认锚定：%q（期望前缀 %q）", s.opts.StateFile, wantDir)
	}
}

// TestEnsureConfigAnchorsToDataDir 验证 ensureConfig 把 config.json 内的
// 相对 rules_path / geo_dir 也锚定到 DataDir（Android 沙箱完整一致性）。
func TestEnsureConfigAnchorsToDataDir(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	cfgJSON := `{"rules_path":"rules.txt","geo_dir":"geo"}`
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0o600); err != nil {
		t.Fatalf("写入测试配置失败：%v", err)
	}

	s := New(Options{ConfigPath: cfgPath, DataDir: dir})
	cfg, err := s.ensureConfig()
	if err != nil {
		t.Fatalf("ensureConfig 失败：%v", err)
	}
	if cfg.RulesPath != filepath.Join(dir, "rules.txt") {
		t.Errorf("RulesPath 未锚定到 DataDir：%q（期望 %q）", cfg.RulesPath, filepath.Join(dir, "rules.txt"))
	}
	if cfg.GeoDir != filepath.Join(dir, "geo") {
		t.Errorf("GeoDir 未锚定到 DataDir：%q（期望 %q）", cfg.GeoDir, filepath.Join(dir, "geo"))
	}
}

// TestResolveExecPathUsesConfigSubdir 验证非 Android（DataDir 空）时，
// resolveExecPath 把相对路径解析到执行根目录下的 config/ 子目录。
func TestResolveExecPathUsesConfigSubdir(t *testing.T) {
	base := t.TempDir()
	exeDir := filepath.Join(base, "bin")
	if err := os.MkdirAll(exeDir, 0o755); err != nil {
		t.Fatalf("创建 exe 目录失败：%v", err)
	}
	origExe, origGetwd := executableDirFn, getwdFn
	executableDirFn = func() (string, error) { return filepath.Join(exeDir, "warp"), nil }
	getwdFn = func() (string, error) { return exeDir, nil }
	defer func() {
		executableDirFn, getwdFn = origExe, origGetwd
	}()

	got := resolveExecPath("config.json")
	want := filepath.Join(exeDir, runtimeConfigDirName, "config.json")
	if got != want {
		t.Errorf("resolveExecPath(\"config.json\") = %q，期望 %q（config/ 子目录）", got, want)
	}
}

// TestBaseExecRootFallsBackToCwd 验证可执行目录不可写（Docker 场景：
// exe 在只读 /usr/local/bin）时，执行根回退到当前工作目录（挂载卷 /data）。
// 这是 Docker 版"文件无法保存"的核心修复。
//
// 不可写通过"exe 目录下 config/ 无法创建"模拟：把 exeDir/config 预置为普通
// 文件（MkdirAll 遇 ENOTDIR 失败）—— 与 root 用户无关，确定性成立。
func TestBaseExecRootFallsBackToCwd(t *testing.T) {
	base := t.TempDir()
	roExe := filepath.Join(base, "ro", "bin")
	if err := os.MkdirAll(roExe, 0o755); err != nil {
		t.Fatalf("创建只读目录失败：%v", err)
	}
	// exeDir/config 被普通文件占位 → config/ 无法作为目录创建 → 判定不可写。
	if err := os.WriteFile(filepath.Join(roExe, runtimeConfigDirName), []byte("x"), 0o644); err != nil {
		t.Fatalf("占位 config 文件失败：%v", err)
	}

	origExe, origGetwd := executableDirFn, getwdFn
	executableDirFn = func() (string, error) { return filepath.Join(roExe, "warp"), nil }
	getwdFn = func() (string, error) { return base, nil }
	defer func() {
		executableDirFn, getwdFn = origExe, origGetwd
	}()

	if got := baseExecRoot(); got != base {
		t.Errorf("baseExecRoot() = %q，期望回退到 cwd %q", got, base)
	}
}

// TestBaseExecRootPrefersWritableExe 验证可执行目录可写时优先使用它
// （便携部署：exe 放哪数据就在哪）。
func TestBaseExecRootPrefersWritableExe(t *testing.T) {
	base := t.TempDir()
	exeDir := filepath.Join(base, "bin")
	if err := os.MkdirAll(exeDir, 0o755); err != nil {
		t.Fatalf("创建 exe 目录失败：%v", err)
	}

	origExe, origGetwd := executableDirFn, getwdFn
	executableDirFn = func() (string, error) { return filepath.Join(exeDir, "warp"), nil }
	getwdFn = func() (string, error) { return base, nil }
	defer func() {
		executableDirFn, getwdFn = origExe, origGetwd
	}()

	if got := baseExecRoot(); got != exeDir {
		t.Errorf("baseExecRoot() = %q，期望可写 exe 目录 %q", got, exeDir)
	}
}

// TestNewAutoCreatesRuntimeDir 验证 New() 在 DataDir 为空（桌面/Docker）时
// 自动创建 config/ 运行时目录，保证 -reg / -geo-update 无需 Start 即能写盘。
func TestNewAutoCreatesRuntimeDir(t *testing.T) {
	base := t.TempDir()
	exeDir := filepath.Join(base, "bin")
	if err := os.MkdirAll(exeDir, 0o755); err != nil {
		t.Fatalf("创建 exe 目录失败：%v", err)
	}
	origExe, origGetwd := executableDirFn, getwdFn
	executableDirFn = func() (string, error) { return filepath.Join(exeDir, "warp"), nil }
	getwdFn = func() (string, error) { return exeDir, nil }
	defer func() {
		executableDirFn, getwdFn = origExe, origGetwd
	}()

	New(Options{})
	configDir := filepath.Join(exeDir, runtimeConfigDirName)
	if fi, err := os.Stat(configDir); err != nil || !fi.IsDir() {
		t.Errorf("New() 未自动创建运行时目录 %s：%v", configDir, err)
	}
}

// TestNewAutoCreatesRuntimeDirDoesNotTouchAndroid 验证 DataDir 非空（Android）
// 时 New() 不创建 config/ 子目录、也不迁移（保持沙箱根锚定）。
func TestNewAutoCreatesRuntimeDirDoesNotTouchAndroid(t *testing.T) {
	sandbox := t.TempDir()
	s := New(Options{DataDir: sandbox})
	if s.opts.ConfigPath != filepath.Join(sandbox, "config.json") {
		t.Errorf("Android ConfigPath 应锚定沙箱根：%q（期望 %q）", s.opts.ConfigPath, filepath.Join(sandbox, "config.json"))
	}
	if _, err := os.Stat(filepath.Join(sandbox, runtimeConfigDirName)); err == nil {
		t.Errorf("Android 不应创建 config/ 子目录：%s 存在", filepath.Join(sandbox, runtimeConfigDirName))
	}
}

// TestLegacyMigrationCopies 验证一次性迁移：旧执行根下的 config.json /
// reg.json / rules.txt / geo/ 被复制进 config/，原文件保留（非破坏式）。
func TestLegacyMigrationCopies(t *testing.T) {
	base := t.TempDir()
	// 构造旧布局：执行根下散落 config.json / reg.json / rules.txt / geo/。
	oldFiles := map[string]string{
		"config.json": `{"listen_addr":"127.0.0.1:40000"}`,
		"reg.json":    `{"id":"legacy-id"}`,
		"rules.txt":   "proxy,geosite:google\n",
	}
	for name, content := range oldFiles {
		if err := os.WriteFile(filepath.Join(base, name), []byte(content), 0o600); err != nil {
			t.Fatalf("写入旧 %s 失败：%v", name, err)
		}
	}
	geoDir := filepath.Join(base, "geo")
	if err := os.MkdirAll(geoDir, 0o755); err != nil {
		t.Fatalf("创建旧 geo/ 失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(geoDir, "geosite.dat"), []byte("fake-site"), 0o644); err != nil {
		t.Fatalf("写入旧 geosite.dat 失败：%v", err)
	}

	origExe := executableDirFn
	origGetwd := getwdFn
	executableDirFn = func() (string, error) { return filepath.Join(base, "bin", "warp"), nil }
	getwdFn = func() (string, error) { return base, nil }
	defer func() {
		executableDirFn = origExe
		getwdFn = origGetwd
	}()

	if err := migrateLegacyConfig(base); err != nil {
		t.Fatalf("migrateLegacyConfig 失败：%v", err)
	}

	dst := filepath.Join(base, runtimeConfigDirName)
	for name := range oldFiles {
		if _, err := os.Stat(filepath.Join(dst, name)); err != nil {
			t.Errorf("旧文件 %s 未迁移进 config/：%v", name, err)
		}
		if _, err := os.Stat(filepath.Join(base, name)); err != nil {
			t.Errorf("原文件 %s 不应被删除（非破坏式）：%v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "geo", "geosite.dat")); err != nil {
		t.Errorf("geo/geosite.dat 未迁移进 config/：%v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "geo", "geosite.dat")); err != nil {
		t.Errorf("原 geo/geosite.dat 不应被删除：%v", err)
	}
}

// TestLegacyMigrationSkipsWhenConfigExists 验证目标 config/config.json 已存在
// 时迁移为 no-op（幂等，绝不覆盖用户新布局）。
func TestLegacyMigrationSkipsWhenConfigExists(t *testing.T) {
	base := t.TempDir()
	dst := filepath.Join(base, runtimeConfigDirName)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("创建 config/ 失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(dst, "config.json"), []byte(`{"listen_addr":"0.0.0.0:9999"}`), 0o600); err != nil {
		t.Fatalf("写入新 config.json 失败：%v", err)
	}
	// 旧布局也有 config.json，但目标已存在 → 必须跳过。
	if err := os.WriteFile(filepath.Join(base, "config.json"), []byte(`{"listen_addr":"old"}`), 0o600); err != nil {
		t.Fatalf("写入旧 config.json 失败：%v", err)
	}

	if err := migrateLegacyConfig(base); err != nil {
		t.Fatalf("migrateLegacyConfig 失败：%v", err)
	}
	data, err := os.ReadFile(filepath.Join(dst, "config.json"))
	if err != nil {
		t.Fatalf("读取新 config.json 失败：%v", err)
	}
	if string(data) != `{"listen_addr":"0.0.0.0:9999"}` {
		t.Errorf("新 config.json 被覆盖：%s", string(data))
	}
}

// TestLegacyMigrationNoBase 验证迁移在旧文件不存在时静默成功（no-op）。
func TestLegacyMigrationNoBase(t *testing.T) {
	base := t.TempDir()
	if err := migrateLegacyConfig(base); err != nil {
		t.Errorf("空基础目录迁移应 no-op：%v", err)
	}
}

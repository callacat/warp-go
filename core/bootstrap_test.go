package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"warp/route"
)

// TestInitDefaultsBootstraps 验证首启引导全链路：config.json 缺失时生成、
// rules.txt 生成（下载失败回退内置模板）、InitDefaults 不挂起（死锁回归
// 防线——gui/service.go 曾在持有 s.mu 时调用 serverInstance 造成永久死锁）。
func TestInitDefaultsBootstraps(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	rulesPath := filepath.Join(dir, "rules.txt")
	geoDir := filepath.Join(dir, "geo")

	// 预写 config.json 指向临时目录：真实首启中 config.json 由 LoadConfig
	// 生成在执行目录，rules/geo 相对路径按执行目录解析。测试用绝对路径
	// 注入，避免规则/数据落到测试二进制目录。
	pre := `{"rules_path":"` + rulesPath + `","geo_dir":"` + geoDir + `"}`
	if err := os.WriteFile(cfgPath, []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}

	// 预写空 GEO 文件跳过真实下载：CI 无外网/慢网下 4MB×2 下载（超时
	// 5min）会超过下方 30s 等待导致测试挂（GEO 下载路径由 route 包单测
	// 覆盖；此处只验证引导全链路）。空文件使 geoDataPresent 为真 → 跳过
	// 下载；后续引擎加载失败仅降级 rules-only（非致命）。
	if err := os.MkdirAll(geoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"geosite.dat", "geoip-lite.dat"} {
		if err := os.WriteFile(filepath.Join(geoDir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	s := New(Options{ConfigPath: cfgPath, StateFile: filepath.Join(dir, "reg.json")})

	done := make(chan error, 1)
	go func() {
		done <- s.InitDefaults(context.Background())
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("InitDefaults 失败：%v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("InitDefaults 挂起（疑似死锁回归）")
	}

	if _, err := os.Stat(cfgPath); err != nil {
		t.Errorf("config.json 未生成：%v", err)
	}
	data, err := os.ReadFile(rulesPath)
	if err != nil {
		t.Fatalf("rules.txt 未生成：%v", err)
	}
	if _, err := route.ParseRules(string(data)); err != nil {
		t.Errorf("生成的 rules.txt 无法解析：%v", err)
	}
}

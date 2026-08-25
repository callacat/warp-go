package core

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// writeGeoFixtures 在 dir 下写两个 .dat 文件，并把 mtime 拨到 age 前（模拟数据年龄）。
func writeGeoFixtures(t *testing.T, dir string, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-age)
	for _, name := range []string{"geosite.dat", "geoip-lite.dat"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
}

// newGeoTestServer 构造指向临时目录的 Server（config 预写 geo_auto_update_days=7，
// rules.txt 预置避免触发规则下载分支），并注入计数版 geoUpdateFn。
func newGeoTestServer(t *testing.T, dir string, geoAge time.Duration) (*Server, *atomic.Int32) {
	t.Helper()
	cfgPath := filepath.Join(dir, "config.json")
	rulesPath := filepath.Join(dir, "rules.txt")
	geoDir := filepath.Join(dir, "geo")

	pre := `{"rules_path":"` + rulesPath + `","geo_dir":"` + geoDir + `","geo_auto_update_days":7}`
	if err := os.WriteFile(cfgPath, []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}
	if geoAge >= 0 {
		writeGeoFixtures(t, geoDir, geoAge) // 负数=不预置数据（缺失场景）
	}
	if err := os.WriteFile(rulesPath, []byte("rules"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(Options{ConfigPath: cfgPath, StateFile: filepath.Join(dir, "reg.json")})
	var calls atomic.Int32
	s.geoUpdateFn = func(ctx context.Context) (bool, error) {
		calls.Add(1)
		if geoAge > 0 {
			writeGeoFixtures(t, geoDir, 0) // 模拟成功更新：mtime 刷新到现在
		}
		return true, nil
	}
	return s, &calls
}

// TestInitDefaultsUpdatesStaleGeo 验证 GUI/Android 每次打开的必经路径
// （InitDefaults）在数据过期时触发更新——修复"只有首次缺失才下载、
// 之后永不自动更新"的主场景。
func TestInitDefaultsUpdatesStaleGeo(t *testing.T) {
	s, calls := newGeoTestServer(t, t.TempDir(), 8*24*time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.InitDefaults(ctx); err != nil {
		t.Fatalf("InitDefaults: %v", err)
	}
	if calls.Load() == 0 {
		t.Fatal("过期数据应在 InitDefaults 中触发一次 GEO 更新")
	}
}

// TestInitDefaultsUpdatesMissingGeo 验证数据缺失场景仍会下载（原有行为回归防线）。
func TestInitDefaultsUpdatesMissingGeo(t *testing.T) {
	s, calls := newGeoTestServer(t, t.TempDir(), -1)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.InitDefaults(ctx); err != nil {
		t.Fatalf("InitDefaults: %v", err)
	}
	if calls.Load() == 0 {
		t.Fatal("数据缺失应触发 GEO 下载")
	}
}

// TestInitDefaultsSkipsFreshGeo 验证新鲜数据不触发冗余下载。
func TestInitDefaultsSkipsFreshGeo(t *testing.T) {
	s, calls := newGeoTestServer(t, t.TempDir(), time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.InitDefaults(ctx); err != nil {
		t.Fatalf("InitDefaults: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("新鲜数据不应触发 GEO 更新")
	}
}

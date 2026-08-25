package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestGeoWaitUntil 验证周期等待时长的纯函数计算：以落盘时间为基准跨进程
// 累计，无记录或已到期立即返回 0（补跑）。
func TestGeoWaitUntil(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour

	cases := []struct {
		name     string
		last     time.Time // 落盘时间；零值 = 无记录
		interval time.Duration
		wantZero bool
		want     time.Duration // wantZero=false 时的精确期望
	}{
		{
			name:     "无落盘记录_立即到期",
			last:     time.Time{},
			interval: 7 * day,
			wantZero: true,
		},
		{
			name:     "未到期_返回剩余时长",
			last:     now.Add(-3 * day),
			interval: 7 * day,
			want:     4 * day,
		},
		{
			name:     "已过期_立即到期",
			last:     now.Add(-8 * day),
			interval: 7 * day,
			wantZero: true,
		},
		{
			name:     "恰好到期_立即更新",
			last:     now.Add(-7 * day),
			interval: 7 * day,
			wantZero: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := geoWaitUntil(now, tc.last, tc.interval)
			if tc.wantZero {
				if got != 0 {
					t.Fatalf("geoWaitUntil() = %v，期望 0（立即到期）", got)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("geoWaitUntil() = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// TestGeoLastUpdate 验证 mtime 基准的"上次成功更新时间"：取两个 .dat 中
// 较旧者；任一缺失视为零值（已到期）。
func TestGeoLastUpdate(t *testing.T) {
	dir := t.TempDir()
	old := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)

	writeWithMtime := func(name string, mt time.Time) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("取较旧者", func(t *testing.T) {
		writeWithMtime("geosite.dat", recent)
		writeWithMtime("geoip-lite.dat", old)
		if got := geoLastUpdate(dir); !got.Equal(old) {
			t.Fatalf("geoLastUpdate() = %v，期望较旧的 %v", got, old)
		}
	})

	t.Run("任一缺失_返回零值", func(t *testing.T) {
		if err := os.Remove(filepath.Join(dir, "geoip-lite.dat")); err != nil {
			t.Fatal(err)
		}
		if got := geoLastUpdate(dir); !got.IsZero() {
			t.Fatalf("缺文件时 geoLastUpdate() = %v，期望零值", got)
		}
	})
}

// TestGeoAutoUpdateLoopCancel 冒烟验证循环在 ctx 取消后退出（不触发更新、
// 不泄漏 goroutine）。到期触发的行为由 geoWaitUntil 单测覆盖数学部分。
func TestGeoAutoUpdateLoopCancel(t *testing.T) {
	s := &Server{} // 循环内不会走到 geoUpdateOnce（等待远大于测试时长）
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		s.geoAutoUpdateLoop(ctx, dir, time.Hour) // 1h 后才到期
		close(done)
	}()

	cancel()
	select {
	case <-done:
		// 正常退出
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 geoAutoUpdateLoop 未退出")
	}
}

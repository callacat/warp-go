package route

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestDownloadDefaultRulesOffline 覆盖默认规则下载全链路（httptest 离线模拟）：
// 首次下载 → SHA-1 去重 → 非法内容拒绝落盘 → 404 报错。
func TestDownloadDefaultRulesOffline(t *testing.T) {
	silenceLogs(t)
	validRules := "REJECT,geosite:category-ads-all\ndirect,geoip:cn\n"

	var body []byte
	mux := http.NewServeMux()
	mux.HandleFunc("/rules.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	})
	mux.HandleFunc("/broken.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("this is not a rule line\n"))
	})
	mux.HandleFunc("/missing.txt", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.txt")

	// 1) 首次下载：文件不存在 → 下载 + ParseRules 校验 + 落盘，updated=true
	body = []byte(validRules)
	updated, err := DownloadDefaultRules(ctx, path, srv.URL+"/rules.txt")
	if err != nil {
		t.Fatalf("首次下载失败：%v", err)
	}
	if !updated {
		t.Fatal("首次下载应返回 updated=true")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("rules.txt 未落盘：%v", err)
	}
	if string(data) != validRules {
		t.Errorf("rules.txt 内容 = %q，期望 %q", data, validRules)
	}

	// 2) 内容未变 → SHA-1 去重跳过，updated=false
	updated, err = DownloadDefaultRules(ctx, path, srv.URL+"/rules.txt")
	if err != nil {
		t.Fatalf("二次下载失败：%v", err)
	}
	if updated {
		t.Fatal("内容未变时应返回 updated=false（SHA-1 去重）")
	}

	// 3) 非法内容 → ParseRules 校验失败，拒绝落盘，旧文件保留
	body = []byte("garbage\nnot-a-rule\n")
	updated, err = DownloadDefaultRules(ctx, path, srv.URL+"/rules.txt")
	if err == nil {
		t.Fatal("非法规则内容应报错")
	}
	data, _ = os.ReadFile(path)
	if string(data) != validRules {
		t.Errorf("校验失败后旧文件被覆盖：%q", data)
	}

	// 4) 404 → 下载错误，不落盘
	updated, err = DownloadDefaultRules(ctx, path, srv.URL+"/missing.txt")
	if err == nil {
		t.Fatal("404 应报错")
	}
	if updated {
		t.Fatal("404 不应报告 updated=true")
	}
}

package route

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/v2fly/v2ray-core/v5/app/router/routercommon"
	"google.golang.org/protobuf/proto"
)

// testGeoSiteData 构造一个合成 GeoSiteList（覆盖全部四种域名类型 + 混合大小写类别名），
// 供测试走真实 protobuf 解析路径。
func testGeoSiteData() []byte {
	list := &routercommon.GeoSiteList{
		Entry: []*routercommon.GeoSite{
			{
				CountryCode: "google", // 小写类别名：验证大小写归一化
				Domain: []*routercommon.Domain{
					{Type: routercommon.Domain_RootDomain, Value: "google.com"},
					{Type: routercommon.Domain_RootDomain, Value: "youtube.com"},
					{Type: routercommon.Domain_Full, Value: "gstatic.com"},
					{Type: routercommon.Domain_Plain, Value: "doubleclick"},
					{Type: routercommon.Domain_Regex, Value: `\.googlevideo\.com$`},
				},
			},
			{
				CountryCode: "PRIVATE",
				Domain: []*routercommon.Domain{
					{Type: routercommon.Domain_RootDomain, Value: "localhost"},
					{Type: routercommon.Domain_RootDomain, Value: "internal.corp"},
				},
			},
			{
				CountryCode: "cn",
				Domain: []*routercommon.Domain{
					{Type: routercommon.Domain_RootDomain, Value: "qq.com"},
					{Type: routercommon.Domain_RootDomain, Value: "baidu.com"},
				},
			},
		},
	}
	data, err := proto.Marshal(list)
	if err != nil {
		panic(err)
	}
	return data
}

// testGeoIPData 构造一个合成 GeoIPList（含 PRIVATE 类别，对应 geoip:private 语义）。
func testGeoIPData() []byte {
	list := &routercommon.GeoIPList{
		Entry: []*routercommon.GeoIP{
			{
				CountryCode: "CN",
				Cidr: []*routercommon.CIDR{
					{Ip: net.ParseIP("1.0.1.0").To4(), Prefix: 24},
					{Ip: net.ParseIP("223.255.252.0").To4(), Prefix: 22},
					{Ip: net.ParseIP("2001:250::").To16(), Prefix: 32},
				},
			},
			{
				CountryCode: "private",
				Cidr: []*routercommon.CIDR{
					{Ip: net.ParseIP("10.0.0.0").To4(), Prefix: 8},
					{Ip: net.ParseIP("192.168.0.0").To4(), Prefix: 16},
					{Ip: net.ParseIP("172.16.0.0").To4(), Prefix: 12},
				},
			},
			{
				CountryCode: "telegram",
				Cidr: []*routercommon.CIDR{
					{Ip: net.ParseIP("91.108.4.0").To4(), Prefix: 22},
					{Ip: net.ParseIP("149.154.160.0").To4(), Prefix: 20},
				},
			},
		},
	}
	data, err := proto.Marshal(list)
	if err != nil {
		panic(err)
	}
	return data
}

// writeTestGeoData 把合成 GEO 数据写入临时目录，返回 geoDir。
func writeTestGeoData(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "geosite.dat"), testGeoSiteData(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "geoip-lite.dat"), testGeoIPData(), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadGeoSite(t *testing.T) {
	dir := writeTestGeoData(t)
	db, err := LoadGeoSite(filepath.Join(dir, "geosite.dat"))
	if err != nil {
		t.Fatalf("LoadGeoSite 失败：%v", err)
	}
	if got := db.CategoryCount(); got != 3 {
		t.Fatalf("类别数 = %d，期望 3", got)
	}

	// 大小写不敏感查询
	for _, name := range []string{"google", "GOOGLE", "Google"} {
		if idx, ok := db.Lookup(name); !ok || idx.domainCount() != 5 {
			t.Errorf("Lookup(%q) 应命中 5 条域名，得到 ok=%v count=%d", name, ok, idx.domainCount())
		}
	}
	if _, ok := db.Lookup("不存在"); ok {
		t.Error("不存在的类别应未命中")
	}
	// PRIVATE 类别名混合大小写存储
	if idx, ok := db.Lookup("private"); !ok || idx.domainCount() != 2 {
		t.Errorf("Lookup(private) 应命中 2 条，得到 ok=%v count=%d", ok, idx.domainCount())
	}
}

func TestLoadGeoIP(t *testing.T) {
	dir := writeTestGeoData(t)
	db, err := LoadGeoIP(filepath.Join(dir, "geoip-lite.dat"))
	if err != nil {
		t.Fatalf("LoadGeoIP 失败：%v", err)
	}
	if got := db.CategoryCount(); got != 3 {
		t.Fatalf("类别数 = %d，期望 3（CN/private/telegram）", got)
	}
	if got := db.PrefixCount(); got != 8 {
		t.Fatalf("前缀总数 = %d，期望 8", got)
	}

	cases := []struct {
		cat  string
		ip   string
		want bool
	}{
		{"cn", "1.0.1.5", true},
		{"CN", "1.0.1.5", true}, // 大小写不敏感
		{"cn", "1.0.2.1", false},
		{"cn", "8.8.8.8", false},
		{"cn", "223.255.253.7", true},
		{"cn", "223.255.255.255", true}, // /22 边界：223.255.252.0/22 覆盖到 223.255.255.255
		{"cn", "2001:250::1", true},
		{"private", "10.1.2.3", true},
		{"private", "192.168.0.1", true},
		{"private", "172.20.0.1", true},
		{"private", "8.8.8.8", false},
		{"telegram", "149.154.175.100", true}, // 149.154.160.0/20
		{"telegram", "91.108.5.7", true},      // 91.108.4.0/22
		{"telegram", "8.8.8.8", false},
		{"missing", "1.0.1.5", false},
	}
	for _, tc := range cases {
		ip := netip.MustParseAddr(tc.ip)
		if got := db.Contains(tc.cat, ip); got != tc.want {
			t.Errorf("Contains(%q, %s) = %v，期望 %v", tc.cat, tc.ip, got, tc.want)
		}
	}
}

func TestLoadGeoDataCorrupt(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.dat")
	os.WriteFile(bad, []byte("这不是 protobuf"), 0o644)

	if _, err := LoadGeoSite(bad); err == nil {
		t.Error("损坏的 geosite 数据应报错")
	}
	if _, err := LoadGeoIP(bad); err == nil {
		t.Error("损坏的 geoip 数据应报错")
	}
	if _, err := LoadGeoSite(filepath.Join(dir, "missing.dat")); err == nil {
		t.Error("缺失文件应报错")
	}
}

// TestUpdateGeoDataOffline 用本地 httptest 服务器模拟 GEO 发布端点，
// 覆盖 首次下载 → SHA-1 去重跳过 → 损坏数据校验拒绝 → 404 报错 全链路。
func TestUpdateGeoDataOffline(t *testing.T) {
	silenceLogs(t)
	validSite := testGeoSiteData()
	validIP := testGeoIPData()

	var siteBody, ipBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("/geosite.dat", func(w http.ResponseWriter, r *http.Request) {
		w.Write(siteBody)
	})
	mux.HandleFunc("/geoip-lite.dat", func(w http.ResponseWriter, r *http.Request) {
		w.Write(ipBody)
	})
	mux.HandleFunc("/broken.dat", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("bad data"))
	})
	mux.HandleFunc("/missing.dat", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	geoDir := t.TempDir()
	ctx := context.Background()

	// 1) 首次更新：文件不存在 → 下载 + 校验 + 落盘，updated=true
	siteBody, ipBody = validSite, validIP
	updated, err := UpdateGeoData(ctx, geoDir, srv.URL+"/geosite.dat", srv.URL+"/geoip-lite.dat")
	if err != nil {
		t.Fatalf("首次更新失败：%v", err)
	}
	if !updated {
		t.Fatal("首次更新应返回 updated=true")
	}
	if _, err := os.Stat(filepath.Join(geoDir, "geosite.dat")); err != nil {
		t.Errorf("geosite.dat 未落盘：%v", err)
	}

	// 2) 内容未变 → SHA-1 去重跳过，updated=false，文件 mtime 不变
	before, _ := os.Stat(filepath.Join(geoDir, "geosite.dat"))
	updated, err = UpdateGeoData(ctx, geoDir, srv.URL+"/geosite.dat", srv.URL+"/geoip-lite.dat")
	if err != nil {
		t.Fatalf("二次更新失败：%v", err)
	}
	if updated {
		t.Fatal("内容未变时应返回 updated=false（SHA-1 去重）")
	}
	after, _ := os.Stat(filepath.Join(geoDir, "geosite.dat"))
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("去重跳过时不应改写文件")
	}

	// 3) 损坏数据 → 校验拒绝报错，且已存在的旧文件保留原样
	siteBody = []byte("corrupted")
	updated, err = UpdateGeoData(ctx, geoDir, srv.URL+"/geosite.dat", srv.URL+"/geoip-lite.dat")
	if err == nil {
		t.Fatal("损坏数据应报错")
	}
	kept, _ := os.ReadFile(filepath.Join(geoDir, "geosite.dat"))
	if string(kept) != string(validSite) {
		t.Error("损坏数据拒绝后旧文件应保留")
	}

	// 4) 404 → 报错且 updated=false
	_, err = UpdateGeoData(ctx, geoDir, srv.URL+"/missing.dat", srv.URL+"/geoip-lite.dat")
	if err == nil {
		t.Fatal("404 应报错")
	}
}

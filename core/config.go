// Package core 提供可复用的 WARP 客户端核心：配置加载、注册编排、
// Server 生命周期（Start/Stop）、状态查询。CLI（main.go）与 GUI（gui/）共用，
// 不依赖任何界面层。
package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"warp/route"
)

// Config 是 config.json 的运行时配置，位于程序执行目录。
//
// 优先级：命令行旗标 > config.json > 默认值。配置文件只在启动/显式保存时
// 读取（v0.5.24 起取消运行中热加载——避免外部编辑与 GUI 保存相互覆盖）。
type Config struct {
	// ListenAddr 是 mixed HTTP+SOCKS5 代理的监听地址。
	ListenAddr string `json:"listen_addr"`
	// EdgeAddr 是扫描应用的最优边缘地址（host:port）；空 = 用注册信息默认。
	// GUI 扫描页"应用"按钮写入；下次启动生效。
	EdgeAddr string `json:"edge_addr,omitempty"`
	// RulesPath 是路由规则文件路径（相对执行目录解析）。
	RulesPath string `json:"rules_path"`
	// GeoDir 是 geosite.dat / geoip-lite.dat 所在目录（相对执行目录解析）。
	GeoDir string `json:"geo_dir"`
	// GeoRepo 是 GEO 数据发布仓库；两个下载 URL 由此推导（见 GeoSiteURL/GeoIPURL）。
	GeoRepo string `json:"geo_repo"`
	// GeoAutoUpdateDays 是 GEO 数据自动更新间隔（天），0 表示关闭。
	GeoAutoUpdateDays int `json:"geo_auto_update_days"`
	// EnableSystemProxy 控制是否把系统代理指向 mixed 代理端口。
	EnableSystemProxy bool `json:"enable_system_proxy"`
	// AllowUDP 控制是否响应 SOCKS5 UDP ASSOCIATE（数据报直连，不经隧道）。
	AllowUDP bool `json:"allow_udp"`
	// DownloadProxy 是 GitHub 下载加速前缀（如 https://gh-proxy.org/）。
	// 仅对 GitHub 官方域名（github.com / raw.githubusercontent.com）的下载
	// URL 生效；置空关闭加速。GEO 数据库与默认规则下载共用。
	DownloadProxy string `json:"download_proxy"`
	// DialTimeoutSeconds 是边缘拨号总超时（秒）。仅 Android 使用：装配
	// ctx 的 WithTimeout 值；0 或缺失 = 默认 60s（见 androidDialTimeoutDefault）。
	DialTimeoutSeconds int `json:"dial_timeout_seconds,omitempty"`
	// TunnelConnections 是隧道 QUIC 连接数（≥1）。>1 时按连接池轮询分发
	// （v0.5.31 吞吐改进：真机单连接被网络按连接限速 ~1Mbps，多连接各自达到
	// 独立均衡、总量可叠加）；0/缺省 = 1。
	TunnelConnections int `json:"tunnel_connections,omitempty"`
	// PhysicalDNS 是物理 DNS 上游列表（v0.5.30 阶段 12 DNS 源分流，Android
	// 专用）：TUN DNS 拦截对国内域名（route→direct）改用物理 DNS 直连解析
	// 拿国内节点。优先级：Java 侧 establish() 前注入的物理网络真实 DNS > 本
	// 字段 > 公共 DNS 兜底（androidvpn/dns.go）。桌面/CLI 无 TUN DNS 拦截，
	// 此字段不生效。
	PhysicalDNS []string `json:"physical_dns,omitempty"`
	// ThemeMode 是界面主题模式：light / dark / system（跟随系统）。
	// GUI 保存设置时同步写入 config.json，启动时从配置读取并注入前端。
	ThemeMode string `json:"theme_mode,omitempty"`
	// PerAppMode 是 Android 分应用代理模式：off=全量代理（默认）| allow=仅
	// 白名单 | disallow=黑名单。桌面端忽略（VpnService 是 Android 概念）；
	// 该值仅持久化，实际过滤由 Java 侧在 establish() 前读取 perapp.json
	// 应用到 VpnService.Builder（见 gui/build/android/.../WarpVpnService.java）。
	PerAppMode string `json:"per_app_mode,omitempty"`
	// PerAppPackages 是分应用代理生效的包名列表（allow/disallow 模式下使用）。
	// 空列表 = 该模式下不过滤（等价全量）。仅 Android 生效。
	PerAppPackages []string `json:"per_app_packages,omitempty"`
}

// DefaultConfig 返回内置默认值。LoadConfig 以它为基底，JSON 反序列化只覆盖
// 文件中出现的字段——因此"缺省"与"显式默认值"等价，"显式 0"仍能关闭自动更新。
func DefaultConfig() *Config {
	return &Config{
		ListenAddr:        "127.0.0.1:40000",
		RulesPath:         "rules.txt",
		GeoDir:            "geo",
		GeoRepo:           "https://github.com/MetaCubeX/meta-rules-dat",
		GeoAutoUpdateDays: 7,
		EnableSystemProxy: false,
		AllowUDP:          false,
		DownloadProxy:     "https://gh-proxy.org/",
		ThemeMode:         "system",
		PerAppMode:        "off",
		TunnelConnections: 2,
	}
}

// AccelerateURL 对 GitHub 官方域名的下载 URL 应用加速前缀；非 GitHub URL
// （镜像仓库、本地测试地址）原样返回。DownloadProxy 为空时关闭加速。
func (c *Config) AccelerateURL(raw string) string {
	proxy := strings.TrimRight(c.DownloadProxy, "/")
	if proxy == "" {
		return raw
	}
	if strings.HasPrefix(raw, "https://github.com/") ||
		strings.HasPrefix(raw, "https://raw.githubusercontent.com/") {
		return proxy + "/" + raw
	}
	return raw
}

// GeoSiteURL 由 GeoRepo 推导 geosite.dat 的下载地址；仓库为空时回退到内置
// 默认。返回的 URL 已应用 DownloadProxy 加速前缀。
func (c *Config) GeoSiteURL() string {
	return c.AccelerateURL(c.geoURL("geosite.dat"))
}

// GeoIPURL 由 GeoRepo 推导 geoip-lite.dat 的下载地址；仓库为空时回退到内置
// 默认。返回的 URL 已应用 DownloadProxy 加速前缀。
func (c *Config) GeoIPURL() string {
	return c.AccelerateURL(c.geoURL("geoip-lite.dat"))
}

func (c *Config) geoURL(name string) string {
	if strings.TrimSpace(c.GeoRepo) == "" {
		if name == "geosite.dat" {
			return route.DefaultGeoSiteURL
		}
		return route.DefaultGeoIPURL
	}
	return strings.TrimRight(c.GeoRepo, "/") + "/releases/download/latest/" + name
}

// LoadConfig 读取并解析 config.json。文件缺失时以默认值原子写入一份再返回
// （首次运行即生成模板）；文件存在但解析失败则报错返回，不改动原文件。
func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if werr := WriteConfig(path, cfg); werr != nil {
				return nil, fmt.Errorf("生成默认配置 %s 失败：%w", path, werr)
			}
			log.Printf("已生成默认配置 %s", path)
			return cfg, nil
		}
		return nil, fmt.Errorf("读取配置 %s 失败：%w", path, err)
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置 %s 失败：%w", path, err)
	}
	return cfg, nil
}

// WriteConfig 以"临时文件 + 改名"原子写入配置，读者永远不会看到半写内容。
// tmp 与目标同目录，保证 rename 在同一文件系统内是原子的。
func WriteConfig(path string, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时文件失败：%w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("写入临时文件失败：%w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败：%w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("设置临时文件权限失败：%w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("原子替换 %s 失败：%w", path, err)
	}
	return nil
}

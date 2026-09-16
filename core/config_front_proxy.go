package core

import "net"

// FrontProxyConfig 是百度中转（保命/备用通道）配置，走 HTTP CONNECT 隧道。
// warp-go 用 QUIC/UDP，HTTP CONNECT 隧道是 TCP——开启 front_proxy 时必须
// 切 TCP fallback 模式（跳过 QUIC），CONNECT 目标=WARP 边缘地址。
//
// 关键互斥：front_proxy 开启时忽略 dialIPs/优选 IP（CONNECT 目标被改写
// 成 CF 优选裸 IP → 百度 503）。GUI 开关需联动置灰优选 IP 输入。
//
// 参考蓝本：x-tunnel internal/app/front_proxy.go（语义移植，不抄实现）。
type FrontProxyConfig struct {
	Enabled     bool   `json:"enabled"`
	Server      string `json:"server"`       // front proxy 地址（host:port）
	ConnectHost string `json:"connect_host"` // Host 头值（兜底=sptest.baidu.com）
	Token       string `json:"token"`        // X-T5-Auth token（可覆写，失效=用户自查）
	UserAgent   string `json:"user_agent"`   // 覆写 UA（空=默认 okhttp）
}

// DefaultFrontProxyConfig 返回内置默认值（百度云加速入口）。
func DefaultFrontProxyConfig() FrontProxyConfig {
	return FrontProxyConfig{
		Enabled:     false,
		Server:      "cloudnproxy.baidu.com:443",
		ConnectHost: "sptest.baidu.com",
		Token:       "482857715",
		UserAgent:   "okhttp/3.11.0 Dalvik/2.1.0 (Linux; Build/RKQ1.200826.002) baiduboxapp/11.0.5.12 (Baidu; P1 11)",
	}
}

// ValidateFrontProxy 校验 front proxy 配置（enabled=false 跳过）。
func ValidateFrontProxy(cfg *FrontProxyConfig) error {
	if cfg == nil || !cfg.Enabled {
		return nil
	}
	server := cfg.Server
	if server == "" {
		return ErrFrontProxyServerEmpty
	}
	// host:port 校验
	host, port, err := net.SplitHostPort(server)
	if err != nil || host == "" || port == "" {
		return ErrFrontProxyServerInvalid
	}
	// connect_host 不能为空（空 Host → 百度 403）
	if cfg.ConnectHost == "" {
		cfg.ConnectHost = "sptest.baidu.com"
	}
	return nil
}

var (
	ErrFrontProxyServerEmpty   = &FrontProxyError{"front_proxy.server 不能为空"}
	ErrFrontProxyServerInvalid = &FrontProxyError{"front_proxy.server 无效（需 host:port）"}
)

type FrontProxyError struct{ msg string }

func (e *FrontProxyError) Error() string { return e.msg }

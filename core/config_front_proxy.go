package core

import (
	"fmt"
	"log"
	"net"
)

// FrontProxyConfig 是百度中转（保命/备用通道）配置，走 HTTP CONNECT 隧道。
// warp-go 用 QUIC/UDP，HTTP CONNECT 隧道是 TCP——开启 front_proxy 时必须
// 切 TCP fallback 模式（跳过 QUIC），CONNECT 目标=用户请求的目标地址。
//
// 关键互斥：front_proxy 开启时忽略 dialIPs/优选 IP（CONNECT 目标被改写成
// CF 优选裸 IP → 百度 503）。GUI 开关需联动置灰优选 IP 输入。
//
// X-T5-Auth token 预填默认凭据（2026-09-17 东哥拍板：与 x-tunnel 同源，开箱
// 即用），用户可在 GUI/配置中覆盖。百度端点对缺失/错误 token 回 403，
// 因此 ValidateFrontProxy 在 enabled 且 token 为空时仍直接报错（防止用户
// 手动清空后误开启）。
//
// DefaultFrontProxyToken 是百度中转共享凭据的默认值（2026-09-17 东哥拍板
// ①：token 空/缺失一律回填该值，开箱即用；与 x-tunnel examples 同源）。
// 单一事实源：前端 types.ts 的 DEFAULT_FRONT_PROXY_TOKEN 与 api.ts 引用
// 均与其对齐，改动时需同步。CHANGELOG 等文档只打码记录，不写全量明文。
const DefaultFrontProxyToken = "482857715"

// DefaultFrontProxyUserAgent 是 front proxy 拨号层默认 UA（与 x-tunnel 同源，
// okhttp/3.11.0 + baiduboxapp 组合串）。真实拨号层在 UA 为空时用它兜底
// （tunnel/front_proxy.go）；GUI mock 展示用同串对齐。
const DefaultFrontProxyUserAgent = "okhttp/3.11.0 Dalvik/2.1.0 (Linux; Build/RKQ1.200826.002) baiduboxapp/11.0.5.12 (Baidu; P1 11)"

// 参考蓝本：x-tunnel internal/app/front_proxy.go（语义移植，不抄实现）。
type FrontProxyConfig struct {
	Enabled     bool   `json:"enabled"`
	Server      string `json:"server"`       // front proxy 地址（host:port）
	ConnectHost string `json:"connect_host"` // Host 头值（兜底=sptest.baidu.com）
	Token       string `json:"token"`        // X-T5-Auth token（预填共享凭据，用户可覆盖）
	UserAgent   string `json:"user_agent"`   // 覆写 UA（空=默认 okhttp）
}

// DefaultFrontProxyConfig 返回内置默认值（百度云加速入口）。
// Token 预填默认凭据（2026-09-17 东哥拍板：与 x-tunnel 同款开箱即用，不再强制用户填）：
// 该 token 为百度云手机 CONNECT 中转共享凭据（x-tunnel examples 同源），
// 失效时由用户/示例文档引导更新。
func DefaultFrontProxyConfig() FrontProxyConfig {
	return FrontProxyConfig{
		Enabled:     false,
		Server:      "cloudnproxy.baidu.com:443",
		ConnectHost: "sptest.baidu.com",
		Token:       DefaultFrontProxyToken,
		UserAgent:   DefaultFrontProxyUserAgent,
	}
}

// ApplyFrontProxyOptions 应用启动期的 front proxy 处理：CLI 旗标三态覆盖 →
// 与优选 IP 的互斥回退 → 配置校验。
//
// 时序是硬约束（C1）：NewKernelContext 读 cfg.FrontProxy.Enabled 决定拨号器
// （FrontProxyDialer vs MasqueClient），ResolveEdgeAddrs 读 opts.EdgeIP 展开
// 边缘候选——两者都在装配段执行，所以这三步必须在 ensureConfig 之后、
// ResolveEdgeAddrs/NewKernel 之前完成。原先它们放在 NewKernel 之后，导致
// CLI -front-proxy 旗标与 EdgeIP 互斥回退对内核装配全部失效（实测：旗标给出
// 后内核全程拨 QUIC，日志 0 处 front proxy 行）。
//
// 配置错误直接返回：启动期暴露，不留到运行时才炸（C2）。
func ApplyFrontProxyOptions(cfg *Config, opts *Options) error {
	if cfg == nil || opts == nil {
		return nil
	}
	// Options.FrontProxyOverride 优先于 config.json（三态语义与 -sysproxy 一致：
	// 旗标给出时强制，未给出按 config.json）。
	if opts.FrontProxyOverride != nil {
		cfg.FrontProxy.Enabled = *opts.FrontProxyOverride
		if *opts.FrontProxyOverride {
			log.Println("✓ front proxy 已通过 -front-proxy 旗标启用")
		}
	}
	// 互斥：front proxy 开启 → EdgeIP 回退 auto（忽略用户指定的优选 IP/边缘）
	// CONNECT 目标被改写成 CF 优选裸 IP → 百度 503（实锤坑）。回退必须发生在
	// ResolveEdgeAddrs 之前，否则边缘候选仍按优选 IP 展开（C1）。
	if cfg.FrontProxy.Enabled && opts.EdgeIP != EdgeIPAuto {
		log.Printf("⚠ front proxy 开启 → EdgeIP 从 %q 回退 auto（互斥：优选 IP 与百度代理冲突）", opts.EdgeIP)
		opts.EdgeIP = EdgeIPAuto
	}
	if err := ValidateFrontProxy(&cfg.FrontProxy); err != nil {
		return fmt.Errorf("front_proxy 配置无效：%w", err)
	}
	return nil
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
	// token 不能为空：X-T5-Auth 缺失/错误时百度端点回 403（实测），
	// 空 token 的配置等于「开启即全 502」，必须在启动期拦住。
	// 注意：默认配置已预填 token，此校验仅覆盖用户手动清空的边界场景。
	if cfg.Token == "" {
		return ErrFrontProxyTokenEmpty
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
	ErrFrontProxyTokenEmpty    = &FrontProxyError{"front_proxy.token 不能为空（X-T5-Auth，缺失时百度端点回 403）"}
)

type FrontProxyError struct{ msg string }

func (e *FrontProxyError) Error() string { return e.msg }

//go:build android || linux

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"

	"warp/androidvpn"
	"warp/core"
	"warp/registration"
)

// builtAndroid 持有 Android 桥启动所需的全部装配产物。vpnCfg 只填静态字段
// （fd/MTU/地址/DNS）；Route/TunnelDial/DirectDial 由桥在创建 core.Kernel
// 之后接线（此时才拿到 kernel.Route / kernel.DialTunnel）。
type builtAndroid struct {
	vpnCfg  androidvpn.Config
	cfg     *core.Config
	regData *registration.Registration
}

// buildAndroidConfig 按 D8 在应用沙箱内装配 Android 启动配置：
//   - config.json 存在则加载（绝对路径，不做执行目录锚定），缺失用默认值
//   - rules.txt / geo 相对路径锚定到沙箱目录
//   - reg.json 缺失时返回错误（镜像 ErrNoRegistration 语义：registration.Load
//     对缺失文件返回 fs.ErrNotExist 包裹错误）
//
// 纯 Go、无 Android 依赖，可在宿主（linux）直接单测。
func buildAndroidConfig(sandboxDir string, fd int) (*builtAndroid, error) {
	if sandboxDir == "" {
		return nil, errors.New("android: 沙箱目录为空")
	}

	configPath := filepath.Join(sandboxDir, "config.json")
	var cfg *core.Config
	if _, err := os.Stat(configPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			cfg = core.DefaultConfig()
		} else {
			return nil, fmt.Errorf("检查配置 %s 失败：%w", configPath, err)
		}
	} else {
		if cfg, err = core.LoadConfig(configPath); err != nil {
			return nil, err
		}
	}

	cfg.RulesPath = joinSandbox(sandboxDir, cfg.RulesPath)
	cfg.GeoDir = joinSandbox(sandboxDir, cfg.GeoDir)

	regPath := filepath.Join(sandboxDir, "reg.json")
	regData, err := registration.Load(regPath)
	if err != nil {
		return nil, fmt.Errorf("没有注册信息（%s），请先在桌面端执行注册：%w", regPath, err)
	}

	vpnCfg := androidvpn.Config{
		FileDescriptor: fd,
		MTU:            1500,
		DNSServers:     []netip.Addr{netip.MustParseAddr("1.1.1.1")},
	}
	if v4, ok := parseAssignedAddr(regData.AssignedIPv4); ok {
		vpnCfg.Inet4Address = []netip.Prefix{netip.PrefixFrom(v4, 32)}
	}
	if v6, ok := parseAssignedAddr(regData.AssignedIPv6); ok {
		vpnCfg.Inet6Address = []netip.Prefix{netip.PrefixFrom(v6, 128)}
	}

	return &builtAndroid{vpnCfg: vpnCfg, cfg: cfg, regData: regData}, nil
}

// joinSandbox 把相对路径锚定到沙箱目录；空串或已是绝对路径时原样返回。
func joinSandbox(sandboxDir, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(sandboxDir, p)
}

// parseAssignedAddr 解析注册信息分配的隧道地址；空串或非法时 ok=false
// （与 core.Kernel.AssignedIPv4/6 的容错语义一致）。
func parseAssignedAddr(s string) (netip.Addr, bool) {
	if s == "" {
		return netip.Addr{}, false
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	return a, true
}

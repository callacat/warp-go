//go:build !android

package core

import "warp/sysproxy"

// setSystemProxy 将系统代理指向 addr（enabled 控制启用/清除），非 Android
// 平台委托 sysproxy 包（windows 注册表 / darwin networksetup / linux gsettings）。
func setSystemProxy(addr string, enabled bool) error {
	return sysproxy.Set(addr, enabled)
}

// sysProxyCurrentlyOn 报告系统代理当前是否指向 addr。读取失败返回 false
// （与 sysproxy.Enabled 的语义一致：读不到就当作未开启，避免 GUI 误显示）。
func sysProxyCurrentlyOn(addr string) bool {
	on, err := sysproxy.Enabled(addr)
	return err == nil && on
}

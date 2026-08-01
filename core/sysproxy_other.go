//go:build !android

package core

import "warp/sysproxy"

// setSystemProxy 将系统代理指向 addr（enabled 控制启用/清除），非 Android
// 平台委托 sysproxy 包（windows 注册表 / darwin networksetup / linux gsettings）。
func setSystemProxy(addr string, enabled bool) error {
	return sysproxy.Set(addr, enabled)
}

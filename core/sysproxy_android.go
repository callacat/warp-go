//go:build android

package core

import "log"

// setSystemProxy 在 Android 上为无操作：Android 系统代理由 VPN 服务接管，
// 不存在 gsettings / networksetup / 注册表路径。返回 nil（视为成功，无副作用），
// 使 core 内所有系统代理调用点在 Android 上安全跳过。
func setSystemProxy(addr string, enabled bool) error {
	log.Printf("Android：系统代理由 VPN 接管，跳过系统代理设置（addr=%s enabled=%v）", addr, enabled)
	return nil
}

// sysProxyCurrentlyOn 在 Android 上恒 false：VPN 接管全部流量，无系统代理。
func sysProxyCurrentlyOn(addr string) bool {
	return false
}

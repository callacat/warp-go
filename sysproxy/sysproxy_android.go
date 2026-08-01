//go:build android

package sysproxy

// set 在 Android 上是无操作：Android 系统代理由 VPN 服务接管，没有
// gsettings / networksetup / 注册表可用。返回 nil（视为成功，无副作用），
// 使 sysproxy.Set 在 Android 上安全通过。
func set(host, port string, enabled bool) error {
	return nil
}

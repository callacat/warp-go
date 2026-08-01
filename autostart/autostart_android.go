//go:build android

package autostart

// Android 没有桌面级自启（开机自启由前台服务 / BOOT_COMPLETED 接收器管理），
// 以下均为无操作，使 autostart 公共 API 在 Android 上安全可用。

func enable(execPath string) error {
	return nil
}

func disable() error {
	return nil
}

func enabled() bool {
	return false
}

// Package autostart 设置/清除开机自启（登录后自动启动）。
//
// 各平台实现：
//   - windows：HKCU\...\Run 注册表项（当前用户，无需管理员）
//   - darwin：~/Library/LaunchAgents/com.callacat.warp-go.plist
//   - linux：~/.config/autostart/warp-go.desktop
package autostart

import "fmt"

// Enable 注册开机自启，启动 execPath（通常是可执行文件绝对路径）。
// 重复调用幂等（覆盖为相同配置）。
func Enable(execPath string) error {
	if execPath == "" {
		return fmt.Errorf("自启程序路径为空")
	}
	return enable(execPath)
}

// Disable 移除开机自启。未启用时返回 nil（幂等）。
func Disable() error {
	return disable()
}

// Enabled 报告当前是否已注册自启。
func Enabled() bool {
	return enabled()
}

//go:build windows

package autostart

import (
	"fmt"

	"golang.org/x/sys/windows/registry"
)

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

func enable(execPath string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开 HKCU Run 注册表失败：%w", err)
	}
	defer k.Close()
	// 带引号：路径可能含空格。
	if err := k.SetStringValue("warp-go", `"`+execPath+`"`); err != nil {
		return fmt.Errorf("写入自启注册表项失败：%w", err)
	}
	return nil
}

func disable() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开 HKCU Run 注册表失败：%w", err)
	}
	defer k.Close()
	if err := k.DeleteValue("warp-go"); err != nil && err != registry.ErrNotExist {
		return fmt.Errorf("删除自启注册表项失败：%w", err)
	}
	return nil
}

func enabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue("warp-go")
	return err == nil
}

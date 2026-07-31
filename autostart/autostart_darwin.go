//go:build darwin

package autostart

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	launchAgentDir  = "Library/LaunchAgents"
	launchAgentName = "com.callacat.warp-go.plist"
)

func agentPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("获取用户主目录失败：%w", err)
	}
	return filepath.Join(home, launchAgentDir, launchAgentName), nil
}

func enable(execPath string) error {
	path, err := agentPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建 LaunchAgents 目录失败：%w", err)
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.callacat.warp-go</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
</dict>
</plist>
`, execPath)
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("写入 LaunchAgent plist 失败：%w", err)
	}
	return nil
}

func disable() error {
	path, err := agentPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除 LaunchAgent plist 失败：%w", err)
	}
	return nil
}

func enabled() bool {
	path, err := agentPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

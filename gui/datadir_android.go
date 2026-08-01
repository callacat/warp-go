//go:build android

package main

import "github.com/wailsapp/wails/v3/pkg/application"

// dataDir 返回运行时文件锚定目录。Android 上返回应用沙箱私有内部目录
// （getFilesDir() 的绝对路径，经 Wails 的 StoragePath 桥接获取）：
// core.Options.DataDir 非空时，所有相对运行时路径（config.json /
// reg.json / rules.txt / geo）锚定到该目录，避免落到只读的 /system/bin
// 导致崩溃。桌面端实现在 datadir_other.go（//go:build !android）。
func dataDir() string {
	return application.Android.StoragePath()
}

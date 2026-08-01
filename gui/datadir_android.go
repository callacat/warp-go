//go:build android

package main

import (
	"sync"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// dataDirCache 缓存首次成功取到的沙箱目录。Wails Android bridge 的
// StoragePath() 在桥接状态抖动时可能瞬时返回 ""（mobile_features_android.go:
// androidBridgeString 失败返回 ""），若每次调用都走桥接，配置/注册路径会在
// 页面切换间"消失"（GetStatus 报 serverInstance 错误 → Registered=false）。
var dataDirCache struct {
	sync.Mutex
	v string
}

// dataDir 返回运行时文件锚定目录。Android 上返回应用沙箱私有内部目录
// （getFilesDir() 的绝对路径，经 Wails 的 StoragePath 桥接获取）：
// core.Options.DataDir 非空时，所有相对运行时路径（config.json /
// reg.json / rules.txt / geo）锚定到该目录，避免落到只读的 /system/bin
// 导致崩溃。首次成功后缓存，后续桥接瞬时失败仍返回已缓存目录。
// 桌面端实现在 datadir_other.go（//go:build !android）。
func dataDir() string {
	dataDirCache.Lock()
	if dataDirCache.v != "" {
		d := dataDirCache.v
		dataDirCache.Unlock()
		return d
	}
	dataDirCache.Unlock()

	d := application.Android.StoragePath()
	if d != "" {
		dataDirCache.Lock()
		dataDirCache.v = d
		dataDirCache.Unlock()
	}
	return d
}

// cachedDataDir 返回已缓存的沙箱目录（可能为空：尚未成功获取过）。
// GetStatus 在 serverInstance 失败时用它兜底检查沙箱内 reg.json，
// 避免桥接抖动把已注册状态误报为未注册。
func cachedDataDir() string {
	dataDirCache.Lock()
	defer dataDirCache.Unlock()
	return dataDirCache.v
}

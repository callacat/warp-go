//go:build !android

package main

// dataDir 返回运行时文件锚定目录。桌面端（含 iOS/mobile stub）返回空串：
// core.Options.DataDir 为空时保持默认行为——所有相对运行时路径
// （config.json / reg.json / rules.txt / geo）锚定到可执行文件所在目录
// （桌面便携部署）。Android 实现在 datadir_android.go（//go:build android）。
func dataDir() string {
	return ""
}

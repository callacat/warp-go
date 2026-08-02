package main

// version 由构建时注入（CI build-release.yml 的 -ldflags "-X main.version=..."）。
// 默认 "dev"：本地 go build 未注入时标记为开发版。
var version = "dev"

// VersionString 返回带 v 前缀的完整版本标识（CLI -version / GUI 显示共用）。
// 与 release tag 对齐：v0.5.3 → "v0.5.3"；本地构建 → "dev"。
func VersionString() string {
	if version == "" || version == "dev" {
		return "dev"
	}
	return "v" + version
}

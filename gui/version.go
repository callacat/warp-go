package main

// version 由构建时注入（build-release.yml 的 -ldflags "-X main.version=..."）。
// GUI 与 CLI 版本号同源（同一 release tag）；默认 "dev" 为本地开发构建。
var version = "dev"

// VersionString 返回带 v 前缀的版本标识，供前端设置页展示。
func VersionString() string {
	if version == "" || version == "dev" {
		return "dev"
	}
	return "v" + version
}

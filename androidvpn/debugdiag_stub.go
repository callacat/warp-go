//go:build !debugdiag

// Package androidvpn 的调试收集 no-op 版本：release 构建（无 -tags
// debugdiag）编译本文件，所有函数为空实现，零 IO 零内存，与"正式版隐藏
// 调试功能"的约束一致。契约与 debugdiag.go（-tags debugdiag）完全一致，
// 两个文件必须同步修改。
package androidvpn

// DebugSetDir 在 release 构建中为 no-op。
func DebugSetDir(string) {}

// DebugStop 在 release 构建中为 no-op。
func DebugStop() {}

// logTunnelClosed 在 release 构建中为 no-op。
func logTunnelClosed(string, int64, int64, int, int64, error) {}

// logUDPClosed 在 release 构建中为 no-op。
func logUDPClosed(string, string, int64, error) {}

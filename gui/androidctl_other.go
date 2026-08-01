//go:build !android

package main

import "errors"

// Android 反向 JNI 桥的桌面桩：桌面端无 VpnService，返回明确错误/零值。
func androidRequestVpnStart() error { return errors.New("此操作仅支持 Android 版") }
func androidRequestVpnStop() error  { return nil }
func androidVpnRunning() bool       { return false }
func androidVpnLastError() string   { return "" }

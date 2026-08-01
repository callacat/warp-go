//go:build android && cgo

package main

/*
#include <jni.h>

// Global JavaVM reference for thread attachment. Captured on the first native
// call via GetJavaVM — the same pattern Wails' application_android.go uses
// (its g_jvm is file-local there, so this file carries its own).
static JavaVM* g_jvm = NULL;

// getEnv returns a JNIEnv for the current thread, attaching it when necessary.
// Sets *needsDetach when the caller must call releaseEnv afterwards.
static JNIEnv* getEnv(int* needsDetach) {
    *needsDetach = 0;
    if (g_jvm == NULL) return NULL;
    JNIEnv* env = NULL;
    jint r = (*g_jvm)->GetEnv(g_jvm, (void**)&env, JNI_VERSION_1_6);
    if (r == JNI_EDETACHED) {
        if ((*g_jvm)->AttachCurrentThread(g_jvm, &env, NULL) != 0) {
            return NULL;
        }
        *needsDetach = 1;
    } else if (r != JNI_OK) {
        return NULL;
    }
    return env;
}

static void releaseEnv(int needsDetach) {
    if (needsDetach && g_jvm != NULL) {
        (*g_jvm)->DetachCurrentThread(g_jvm);
    }
}

static void storeJvm(JNIEnv* env) {
    if (g_jvm == NULL) {
        (*env)->GetJavaVM(env, &g_jvm);
    }
}
*/
import "C"

import (
	"context"
	"log"
	"sync"

	"github.com/wailsapp/wails/v3/pkg/application"

	"warp/androidvpn"
	"warp/core"
)

// androidRuntime 是 Android 桥的包级单例：同一时刻只运行一个 VPN 实例。
// mu 保护全部字段；nativeStopVpn 与 nativeStartVpn 可能从不同线程调用。
var androidRuntime struct {
	mu      sync.Mutex
	kernel  *core.Kernel
	vpn     *androidvpn.Vpn
	cancel  context.CancelFunc
	started bool
}

// Java_com_wails_app_WarpVpnService_nativeStartVpn 是 Java 侧
// WarpVpnService 的 JNI 入口：VpnService.Builder.establish() 拿到 TUN fd 后
// 传入，Go 侧装配 core.Kernel（MASQUE 隧道 + 分流引擎）与 androidvpn 栈并启动。
//
// 返回 0 表示成功；-1 表示失败（配置缺失、Kernel 建立失败等）或已在运行。
//
//export Java_com_wails_app_WarpVpnService_nativeStartVpn
func Java_com_wails_app_WarpVpnService_nativeStartVpn(env *C.JNIEnv, obj C.jobject, fd C.jint) C.jint {
	C.storeJvm(env)

	androidRuntime.mu.Lock()
	if androidRuntime.started {
		androidRuntime.mu.Unlock()
		log.Println("⚠ nativeStartVpn：VPN 已在运行")
		return -1
	}
	androidRuntime.mu.Unlock()

	if int(fd) <= 0 {
		log.Printf("⚠ nativeStartVpn：无效的 TUN fd=%d", int(fd))
		return -1
	}

	sandboxDir := application.Android.StoragePath()
	if sandboxDir == "" {
		log.Println("⚠ nativeStartVpn：无法获取应用沙箱目录")
		return -1
	}

	built, err := buildAndroidConfig(sandboxDir, int(fd))
	if err != nil {
		log.Printf("⚠ nativeStartVpn：配置装配失败：%v", err)
		return -1
	}

	edgeAddrs, err := core.ResolveEdgeAddrs(built.cfg, "", "", built.regData)
	if err != nil {
		log.Printf("⚠ nativeStartVpn：边缘地址解析失败：%v", err)
		return -1
	}
	tlsConfig, err := core.BuildTLSConfig(built.regData)
	if err != nil {
		log.Printf("⚠ nativeStartVpn：TLS 配置失败：%v", err)
		return -1
	}

	kernel, err := core.NewKernel(built.cfg, built.regData, edgeAddrs, tlsConfig)
	if err != nil {
		log.Printf("⚠ nativeStartVpn：Kernel 建立失败：%v", err)
		return -1
	}

	// 接线分流与拨号：proxy → 隧道；direct → 本地直连（DirectDial 留 nil，
	// androidvpn 内用 net.Dialer 默认）。TUN 目标为 IP 字面量时传真实 IP，
	// 使 geoip 规则可命中。
	built.vpnCfg.Route = kernel.Route
	built.vpnCfg.TunnelDial = kernel.DialTunnel
	built.vpnCfg.DirectDial = nil

	vpn, err := androidvpn.New(built.vpnCfg)
	if err != nil {
		log.Printf("⚠ nativeStartVpn：androidvpn 初始化失败：%v", err)
		_ = kernel.Close()
		return -1
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = kernel.Start(ctx) }()
	go func() { _ = vpn.Start(ctx) }()

	androidRuntime.mu.Lock()
	androidRuntime.kernel = kernel
	androidRuntime.vpn = vpn
	androidRuntime.cancel = cancel
	androidRuntime.started = true
	androidRuntime.mu.Unlock()

	log.Printf("✓ VPN 已启动（fd=%d）", int(fd))
	return 0
}

// Java_com_wails_app_WarpVpnService_nativeStopVpn 停止运行中的 VPN 并拆除
// Kernel（幂等：未运行时直接返回 0）。
//
//export Java_com_wails_app_WarpVpnService_nativeStopVpn
func Java_com_wails_app_WarpVpnService_nativeStopVpn(env *C.JNIEnv, obj C.jobject) C.jint {
	androidRuntime.mu.Lock()
	kernel := androidRuntime.kernel
	vpn := androidRuntime.vpn
	cancel := androidRuntime.cancel
	androidRuntime.kernel = nil
	androidRuntime.vpn = nil
	androidRuntime.cancel = nil
	androidRuntime.started = false
	androidRuntime.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if vpn != nil {
		if err := vpn.Stop(); err != nil {
			log.Printf("⚠ nativeStopVpn：停止 TUN 失败：%v", err)
		}
	}
	if kernel != nil {
		if err := kernel.Close(); err != nil {
			log.Printf("⚠ nativeStopVpn：关闭 Kernel 失败：%v", err)
		}
	}
	log.Println("✓ VPN 已停止")
	return 0
}

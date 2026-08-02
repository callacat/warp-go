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

// Reverse-JNI bridge primitives. JNIEnv calls must live in the C preamble
// (cgo does not support the -> operator in Go code) — mirror of Wails'
// storeBridgeRef helper pattern.
static jclass newGlobalRef(JNIEnv* env, jclass cls) {
    return (*env)->NewGlobalRef(env, cls);
}

static jmethodID getRequestStartMethod(JNIEnv* env, jclass cls) {
    return (*env)->GetStaticMethodID(env, cls, "requestStartVpn", "()V");
}

static jmethodID getRequestStopMethod(JNIEnv* env, jclass cls) {
    return (*env)->GetStaticMethodID(env, cls, "requestStopVpn", "()V");
}

static void callStaticVoidMethod(JNIEnv* env, jclass cls, jmethodID mid) {
    (*env)->CallStaticVoidMethod(env, cls, mid);
}
*/
import "C"

import (
	"context"
	"errors"
	"log"
	"sync"
	"unsafe"

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
	lastErr string
}

// androidCtl 持有反向 JNI 桥的 MainActivity 全局引用与方法 ID。
// nativeBridgeReady 在 Java 主线程（onCreate）缓存它们，避免从任意 Go
// goroutine FindClass 错失应用 classloader。
var androidCtl struct {
	mu     sync.Mutex
	cls    C.jclass // MainActivity 全局引用
	startM C.jmethodID
	stopM  C.jmethodID
	ready  bool
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

	// rollback 在 kernel/vpn 任一异步启动失败时拆除本实例并回滚状态。
	// 闭包捕获本地变量而非 androidRuntime 字段：若回滚前已有新实例
	// （started 被再次置 true），旧实例的拆除不碰新实例状态。
	rollback := func(name string, err error) {
		log.Printf("⚠ nativeStartVpn：%s 启动失败，回滚：%v", name, err)
		cancel()
		if vpn != nil {
			_ = vpn.Stop()
		}
		if kernel != nil {
			_ = kernel.Close()
		}
		androidRuntime.mu.Lock()
		if androidRuntime.kernel == kernel {
			androidRuntime.kernel = nil
			androidRuntime.vpn = nil
			androidRuntime.cancel = nil
			androidRuntime.started = false
		}
		androidRuntime.lastErr = err.Error()
		androidRuntime.mu.Unlock()
	}

	// 先赋值再启动：started 在 goroutine 可能完成前即为 true，防并发
	// 重复启动（Java 侧 nativeStartVpn 串行，但 Go 侧可能被并发调用）。
	androidRuntime.mu.Lock()
	androidRuntime.kernel = kernel
	androidRuntime.vpn = vpn
	androidRuntime.cancel = cancel
	androidRuntime.lastErr = ""
	androidRuntime.started = true
	androidRuntime.mu.Unlock()

	go func() {
		if err := kernel.Start(ctx); err != nil {
			rollback("kernel", err)
		}
	}()
	go func() {
		if err := vpn.Start(ctx); err != nil {
			rollback("TUN 栈", err)
		}
	}()

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

// Java_com_wails_app_MainActivity_nativeBridgeReady 是 MainActivity.onCreate
// 的反向桥初始化：在 Java 主线程缓存 MainActivity 全局引用与静态方法 ID，
// 使任意 Go goroutine 都能安全地请求 VPN 启动/停止。
//
//export Java_com_wails_app_MainActivity_nativeBridgeReady
func Java_com_wails_app_MainActivity_nativeBridgeReady(env *C.JNIEnv, cls C.jclass) C.jint {
	C.storeJvm(env)
	clsRef := C.newGlobalRef(env, cls)
	if unsafe.Pointer(clsRef) == nil {
		log.Println("⚠ nativeBridgeReady：无法创建 MainActivity 全局引用")
		return -1
	}
	startM := C.getRequestStartMethod(env, clsRef)
	stopM := C.getRequestStopMethod(env, clsRef)
	if unsafe.Pointer(startM) == nil || unsafe.Pointer(stopM) == nil {
		log.Println("⚠ nativeBridgeReady：找不到 requestStartVpn/requestStopVpn 静态方法")
		return -1
	}
	androidCtl.mu.Lock()
	androidCtl.cls = clsRef
	androidCtl.startM = startM
	androidCtl.stopM = stopM
	androidCtl.ready = true
	androidCtl.mu.Unlock()
	log.Println("✓ Android 反向 JNI 桥就绪（requestStartVpn/requestStopVpn）")
	return 0
}

// androidRequestVpnStart 请求 Java 侧启动 VPN（consent 流 + VpnService）。
func androidRequestVpnStart() error {
	androidCtl.mu.Lock()
	cls, startM, ready := androidCtl.cls, androidCtl.startM, androidCtl.ready
	androidCtl.mu.Unlock()
	if !ready || unsafe.Pointer(cls) == nil || unsafe.Pointer(startM) == nil {
		return errors.New("Android VPN 桥未就绪（MainActivity 未初始化）")
	}
	var needsDetach C.int
	env := C.getEnv(&needsDetach)
	if env == nil {
		return errors.New("Android VPN 桥：无法获取 JNIEnv")
	}
	defer C.releaseEnv(needsDetach)
	C.callStaticVoidMethod(env, cls, startM)
	return nil
}

// androidRequestVpnStop 请求 Java 侧停止 VPN。
func androidRequestVpnStop() error {
	androidCtl.mu.Lock()
	cls, stopM, ready := androidCtl.cls, androidCtl.stopM, androidCtl.ready
	androidCtl.mu.Unlock()
	if !ready || unsafe.Pointer(cls) == nil || unsafe.Pointer(stopM) == nil {
		return nil // 桥未就绪 = 尚未启动，停止为幂等 no-op
	}
	var needsDetach C.int
	env := C.getEnv(&needsDetach)
	if env == nil {
		return errors.New("Android VPN 桥：无法获取 JNIEnv")
	}
	defer C.releaseEnv(needsDetach)
	C.callStaticVoidMethod(env, cls, stopM)
	return nil
}

// androidVpnRunning 报告 VPN 是否运行中（androidRuntime 状态）。
func androidVpnRunning() bool {
	androidRuntime.mu.Lock()
	defer androidRuntime.mu.Unlock()
	return androidRuntime.started
}

// androidVpnLastError 返回最近一次 VPN/内核错误。
func androidVpnLastError() string {
	androidRuntime.mu.Lock()
	defer androidRuntime.mu.Unlock()
	return androidRuntime.lastErr
}

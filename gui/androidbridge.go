//go:build android && cgo

package main

/*
#include <jni.h>
#include <stdlib.h> // C.free（释放 C.CString 分配的 C 字符串）

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

// openExternalBrowser(String) 跳第三方浏览器打开 URL（Android WebView 内
// target=_blank 会被应用内捕获，GitHub 下载页在 WebView 里体验差/登录墙）。
static jmethodID getOpenBrowserMethod(JNIEnv* env, jclass cls) {
    return (*env)->GetStaticMethodID(env, cls, "openExternalBrowser", "(Ljava/lang/String;)V");
}

static void callStaticVoidMethod(JNIEnv* env, jclass cls, jmethodID mid) {
    (*env)->CallStaticVoidMethod(env, cls, mid);
}

static void callStaticVoidMethodStr(JNIEnv* env, jclass cls, jmethodID mid, const char* arg) {
    jstring js = (*env)->NewStringUTF(env, arg);
    if (js == NULL) return;
    (*env)->CallStaticVoidMethod(env, cls, mid, js);
    (*env)->DeleteLocalRef(env, js);
}

// jstring → Go string 的 C 侧转换原语。JNIEnv 调用必须留在 C preamble
// （cgo 不支持 Go 代码里 -> 运算符），与上方 getRequestStartMethod 等一致。
static const char* jstringToChars(JNIEnv* env, jstring j, jboolean* isCopy) {
    if (j == NULL) return NULL;
    return (*env)->GetStringUTFChars(env, j, isCopy);
}

static void releaseChars(JNIEnv* env, jstring j, const char* chars) {
    if (j != NULL && chars != NULL) {
        (*env)->ReleaseStringUTFChars(env, j, chars);
    }
}
*/
import "C"

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"
	_ "time/tzdata" // 内嵌时区数据库：Android 系统无 tzdata，不导入 LoadLocation 必失败
	"unsafe"

	"github.com/wailsapp/wails/v3/pkg/application"

	"warp/androidvpn"
	"warp/core"
)

// androidDialTimeout 是 Android 内核装配的拨号总超时。移动网络下 QUIC/UDP
// 可能被运营商封锁，无限重试只会无限刷错误并让状态停在"连接中"；超时后
// 报明确错误，用户可检查网络后重试。
const androidDialTimeout = 30 * time.Second

// androidRuntime 是 Android 桥的包级单例：同一时刻只运行一个 VPN 实例。
// mu 保护全部字段；nativeStopVpn 与 nativeStartVpn 可能从不同线程调用。
// ctx 是当前实例的装配/生命周期取消上下文（startVpnKernel 用它判断实例
// 是否已被 nativeStopVpn 停止/替换——身份比较，非值比较）。
var androidRuntime struct {
	mu      sync.Mutex
	kernel  *core.Kernel
	vpn     *androidvpn.Vpn
	ctx     context.Context
	cancel  context.CancelFunc
	started bool
	lastErr string
}

// androidCtl 持有反向 JNI 桥的 MainActivity 全局引用与方法 ID。
// nativeBridgeReady 在 Java 主线程（onCreate）缓存它们，避免从任意 Go
// goroutine FindClass 错失应用 classloader。
var androidCtl struct {
	mu           sync.Mutex
	cls          C.jclass // MainActivity 全局引用
	startM       C.jmethodID
	stopM        C.jmethodID
	openBrowserM C.jmethodID
	ready        bool
}

// Java_com_wails_app_WarpVpnService_nativeStartVpn 是 Java 侧
// WarpVpnService 的 JNI 入口：VpnService.Builder.establish() 拿到 TUN fd 后
// 传入，Go 侧装配 core.Kernel（MASQUE 隧道 + 分流引擎）与 androidvpn 栈并启动。
//
// 必须在 Java 主线程（onStartCommand）内尽快返回：Kernel 装配里的边缘地址
// 解析（resolveEdge 的 DNS 查找最长 10s）与 MASQUE 拨号（指数退避重试）若
// 阻塞该线程，系统会在 5s 后 ANR"卡死"（v0.5.9 真机反馈：日志停留在
// "TUN established" 后 10s 才有 SIGQUIT dump，nativeStartVpn 处于 RUNNABLE）。
// 因此本函数只做轻量前置校验 + 记录状态，真正的装配与启动全部移入
// goroutine；返回 0 表示"已受理"（Java 侧据此保持 vpnPfd 与前台服务），
// 后续失败经 androidRuntime.lastErr 与 log 上报，由 GetStatus 展示。
//
// 返回 -1 表示前置校验失败（已在运行 / 无效 fd / 无沙箱目录 / 无注册信息）——
// 这些是确定性的快速失败，不涉及网络，可安全同步返回。
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

	// 快速前置校验：注册信息缺失是确定性失败（buildAndroidConfig 返回
	// fs.ErrNotExist 包裹错误），不必启动 goroutine。config.json 与 rules 等
	// 由 goroutine 内完整装配（BuildTLSConfig 的 PeerPublicKeyVerifier 有
	// PEM 解码等少量 CPU 工作，不该阻塞主线程）。
	built, err := buildAndroidConfig(sandboxDir, int(fd))
	if err != nil {
		log.Printf("⚠ nativeStartVpn：配置装配失败：%v", err)
		androidRuntime.mu.Lock()
		androidRuntime.lastErr = err.Error()
		androidRuntime.mu.Unlock()
		return -1
	}

	// 先置 started + 创建装配取消信号：装配在 goroutine 异步进行，此标记
	// 表示"已受理"，闭合竞态——否则装配期间 androidVpnRunning() 返回 false，
	// 用户再点启动会再次触发（Service.Start 的幂等判断失效，产生双 Kernel）。
	// cancel 同时是装配取消信号：nativeStopVpn 在装配完成前到达时取消它，
	// startVpnKernel 每次装配前检查 ctx 已取消则中止（否则用户"启动后立刻
	// 停止"会得到装配照常完成、VPN 仍运行的反直觉结果）。
	// 拨号总超时：移动网络下 QUIC/UDP 可能被运营商封锁，无限指数退避重试
	// 只会无限刷"边缘不可达"并让状态永远停在"连接中"（v0.5.10 反馈）。
	// 30s 后 NewKernelContext 返回 DeadlineExceeded，报明确错误供用户重试。
	ctx, cancel := context.WithTimeout(context.Background(), androidDialTimeout)
	androidRuntime.mu.Lock()
	androidRuntime.ctx = ctx
	androidRuntime.cancel = cancel
	androidRuntime.started = true
	androidRuntime.lastErr = ""
	androidRuntime.mu.Unlock()

	go startVpnKernel(ctx, cancel, sandboxDir, built, int(fd))
	log.Printf("✓ 已受理 VPN 启动（fd=%d，内核装配异步进行）", int(fd))
	return 0
}

// startVpnKernel 在后台 goroutine 装配并启动 Kernel 与 TUN 栈：
//   - 边缘地址解析（可能走 10s DNS，见 resolveEdge）不阻塞 Java 主线程
//   - NewKernel 内部 MASQUE 拨号重试到连通，可能耗时数秒
//
// ctx 是 nativeStartVpn 前置创建的装配/生命周期取消信号：装配各阶段前
// 检查 ctx.Err()，nativeStopVpn 在装配完成前到达（cancel 已调用）则中止。
// cancel 由 rollback（kernel/vpn 异步启动失败时）调用以停止另一组件。
// 失败时经 androidRuntime.lastErr 上报（GetStatus 展示），并回滚已分配资源。
func startVpnKernel(ctx context.Context, cancel context.CancelFunc, sandboxDir string, built *builtAndroid, fd int) {
	if ctx.Err() != nil {
		failStartCtx(ctx, ctx.Err())
		return
	}
	edgeAddrs, err := core.ResolveEdgeAddrs(built.cfg, "", "", built.regData)
	if err != nil {
		log.Printf("⚠ nativeStartVpn：边缘地址解析失败：%v", err)
		failStart("边缘地址解析失败", err)
		return
	}
	if ctx.Err() != nil {
		failStartCtx(ctx, ctx.Err())
		return
	}
	tlsConfig, err := core.BuildTLSConfig(built.regData)
	if err != nil {
		log.Printf("⚠ nativeStartVpn：TLS 配置失败：%v", err)
		failStart("TLS 配置失败", err)
		return
	}
	if ctx.Err() != nil {
		failStartCtx(ctx, ctx.Err())
		return
	}

	// NewKernel 内部 MASQUE 拨号重试到连通（每候选 2s 超时 + 指数退避），
	// 边缘不可达时可能持续数分钟。记录"拨号中"供用户从日志/状态判断，
	// 而非界面毫无反馈地"卡在启动"。
	log.Printf("正在连接 WARP 边缘 %v ...", edgeAddrs)
	kernel, err := core.NewKernelContext(ctx, built.cfg, built.regData, edgeAddrs, tlsConfig)
	if err != nil {
		// ctx 已取消 = 用户装配中点了停止：NewKernelContext 的拨号被中止，
		// 报"已取消"而非误导性的"建立失败"。
		if ctx.Err() != nil {
			failStartCtx(ctx, ctx.Err())
			return
		}
		log.Printf("⚠ nativeStartVpn：Kernel 建立失败：%v", err)
		failStart("Kernel 建立失败", err)
		return
	}
	if ctx.Err() != nil {
		_ = kernel.Close()
		failStartCtx(ctx, ctx.Err())
		return
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
		failStart("androidvpn 初始化失败", err)
		return
	}
	if ctx.Err() != nil {
		_ = kernel.Close()
		_ = vpn.Stop()
		failStartCtx(ctx, ctx.Err())
		return
	}

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
			androidRuntime.ctx = nil
			androidRuntime.cancel = nil
			androidRuntime.started = false
		}
		androidRuntime.lastErr = err.Error()
		androidRuntime.mu.Unlock()
	}

	// 装配完成前的最后一次取消检查：nativeStopVpn 可能已拆除了本实例
	// （cancel 被调用）。此时不得再写入 androidRuntime（会复活已停止的
	// VPN 且泄漏 kernel/vpn），直接拆除本地资源。
	if ctx.Err() != nil {
		_ = kernel.Close()
		_ = vpn.Stop()
		failStartCtx(ctx, ctx.Err())
		return
	}

	androidRuntime.mu.Lock()
	if androidRuntime.ctx != ctx {
		// nativeStopVpn 已更换/清空 ctx（新实例启动或已停止）：本实例
		// 是过期的，不写入运行状态，拆除本地资源。
		androidRuntime.mu.Unlock()
		_ = kernel.Close()
		_ = vpn.Stop()
		log.Println("⚠ nativeStartVpn：实例已过期（期间被停止/重启），丢弃装配结果")
		return
	}
	androidRuntime.kernel = kernel
	androidRuntime.vpn = vpn
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

	log.Printf("✓ VPN 已启动（fd=%d）", fd)
}

// failStart 记录异步装配失败并回滚状态：清 started（允许用户重试）、写
// lastErr（GetStatus 展示）。TUN fd 由 Java 侧持有（ParcelFileDescriptor，
// Go 侧无法关闭）；Java 的 WarpVpnService 以 nativeStartVpn 返回值判断是否
// 持有 fd，异步失败只能靠日志 + lastErr 提示用户，fd 在服务 onDestroy /
// 下次启动替换时由 Java 侧统一关闭。
func failStart(stage string, err error) {
	androidRuntime.mu.Lock()
	androidRuntime.started = false
	androidRuntime.lastErr = err.Error()
	androidRuntime.mu.Unlock()
	log.Printf("⚠ VPN 启动失败（%s）：%v", stage, err)
}

// failStartCtx 区分装配 ctx 的两种中止原因并上报：
//   - DeadlineExceeded → 拨号总超时（30s，见 androidDialTimeout）——边缘
//     不可达被运营商封锁/弱网，报明确错误供用户检查网络后重试，而非
//     无限重试（v0.5.10 反馈"一直边缘不可达 3.2s 后重试"）；
//   - 其余（Canceled）→ 用户装配中点了停止，正常中止。
func failStartCtx(ctx context.Context, err error) {
	if ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		failStart("连接边缘超时", errors.New("30 秒内未能连接 WARP 边缘，请检查网络后重试"))
		return
	}
	failStart("已取消", err)
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
	androidRuntime.ctx = nil
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
	openBrowserM := C.getOpenBrowserMethod(env, clsRef)
	if unsafe.Pointer(startM) == nil || unsafe.Pointer(stopM) == nil || unsafe.Pointer(openBrowserM) == nil {
		log.Println("⚠ nativeBridgeReady：找不到 requestStartVpn/requestStopVpn/openExternalBrowser 静态方法")
		return -1
	}
	androidCtl.mu.Lock()
	androidCtl.cls = clsRef
	androidCtl.startM = startM
	androidCtl.stopM = stopM
	androidCtl.openBrowserM = openBrowserM
	androidCtl.ready = true
	androidCtl.mu.Unlock()
	log.Println("✓ Android 反向 JNI 桥就绪（requestStartVpn/requestStopVpn/openExternalBrowser）")
	return 0
}

// androidOpenExternalBrowser 请求 Java 侧用系统浏览器打开 URL（Android
// WebView 内 target=_blank 会被应用内捕获，GitHub 下载页需跳第三方浏览器）。
func androidOpenExternalBrowser(url string) error {
	androidCtl.mu.Lock()
	cls, mid, ready := androidCtl.cls, androidCtl.openBrowserM, androidCtl.ready
	androidCtl.mu.Unlock()
	if !ready || unsafe.Pointer(cls) == nil || unsafe.Pointer(mid) == nil {
		return errors.New("Android 浏览器桥未就绪（MainActivity 未初始化）")
	}
	var needsDetach C.int
	env := C.getEnv(&needsDetach)
	if env == nil {
		return errors.New("Android 浏览器桥：无法获取 JNIEnv")
	}
	defer C.releaseEnv(needsDetach)
	cstr := C.CString(url)
	C.callStaticVoidMethodStr(env, cls, mid, cstr)
	C.free(unsafe.Pointer(cstr))
	return nil
}

// androidRequestVpnStart 请求 Java 侧启动 VPN（consent 流 + VpnService）。
func androidRequestVpnStart() error {
	androidCtl.mu.Lock()
	cls, startM, ready := androidCtl.cls, androidCtl.startM, androidCtl.ready
	androidCtl.mu.Unlock()
	if !ready || unsafe.Pointer(cls) == nil || unsafe.Pointer(startM) == nil {
		// 桥未就绪 = MainActivity 尚未初始化（首启时序问题）。错误必须
		// 记录到 GUI 日志页（logWriter 环形缓冲），否则用户"点击启动无反应
		// 也没有日志"——前端按钮旁的错误小字在手机上极易忽略。
		err := errors.New("Android VPN 桥未就绪（MainActivity 未初始化，请稍候重试）")
		log.Printf("⚠ 启动失败：%v", err)
		return err
	}
	var needsDetach C.int
	env := C.getEnv(&needsDetach)
	if env == nil {
		err := errors.New("Android VPN 桥：无法获取 JNIEnv")
		log.Printf("⚠ 启动失败：%v", err)
		return err
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

// Java_com_wails_app_MainActivity_nativeLogMessage 是 Java 侧把日志送入
// GUI 环形缓冲的入口：MainActivity.requestStartVpn/requestStopVpn 等 Java
// 路径的失败（sInstance 为 null、consent 拒绝等）只打 logcat，用户看不到
// 也进不了 GUI 日志页。此导出让 Java 侧把关键消息转发到 GUI 日志页。
//
//export Java_com_wails_app_MainActivity_nativeLogMessage
func Java_com_wails_app_MainActivity_nativeLogMessage(env *C.JNIEnv, obj C.jobject, level C.jstring, msg C.jstring) C.jint {
	C.storeJvm(env)
	toGo := func(j C.jstring) string {
		if unsafe.Pointer(j) == nil {
			return ""
		}
		var needsDetach C.int
		e := C.getEnv(&needsDetach)
		if e == nil {
			return ""
		}
		defer C.releaseEnv(needsDetach)
		chars := C.jstringToChars(e, j, nil)
		if chars == nil {
			return ""
		}
		defer C.releaseChars(e, j, chars)
		return C.GoString(chars)
	}
	lvl := toGo(level)
	text := toGo(msg)
	if text == "" {
		return 0
	}
	switch lvl {
	case "error":
		log.Printf("⚠ %s", text)
	case "warn":
		log.Printf("⚠ %s", text)
	default:
		log.Printf("%s", text)
	}
	return 0
}

// Java_com_wails_app_MainActivity_nativeSetTimeZone 同步 Android 系统时区到
// Go 运行时：Android 上 Go 的 time.Local 默认是 UTC，日志时间戳（logs.go
// 的 HH:MM:SS）会与状态栏系统时间不一致（用户报告"日志显示的不是系统
// 时间"）。MainActivity.onCreate 把 TimeZone.getDefault().getID() 传进来，
// 设置 time.Local 后所有日志/文件时间戳都走设备本地时区。
//
//export Java_com_wails_app_MainActivity_nativeSetTimeZone
func Java_com_wails_app_MainActivity_nativeSetTimeZone(env *C.JNIEnv, obj C.jobject, tz C.jstring) C.jint {
	if unsafe.Pointer(tz) == nil {
		return 0
	}
	C.storeJvm(env)
	var needsDetach C.int
	e := C.getEnv(&needsDetach)
	if e == nil {
		return -1
	}
	defer C.releaseEnv(needsDetach)
	chars := C.jstringToChars(e, tz, nil)
	if chars == nil {
		return 0
	}
	defer C.releaseChars(e, tz, chars)
	id := C.GoString(chars)
	if loc, err := time.LoadLocation(id); err == nil {
		time.Local = loc
		log.Printf("✓ 已同步系统时区：%s", id)
	} else {
		log.Printf("⚠ 无法加载时区 %q：%v", id, err)
	}
	return 0
}

// androidVpnRunning 报告 VPN 是否运行中（androidRuntime 状态）。
func androidVpnRunning() bool {
	androidRuntime.mu.Lock()
	defer androidRuntime.mu.Unlock()
	return androidRuntime.started
}

// Java_com_wails_app_WarpVpnService_nativeVpnRunning 是 Java 侧查询 Go 内核
// 运行态的 JNI 入口。WarpVpnService.onStartCommand 的重入守卫需要区分
// "内核真在运行"（START_STICKY 重投/并发 start → 幂等跳过）与"已受理但异步
// 装配失败"（started=false → 释放旧 TUN fd 重新建立）——仅靠 Java 本地
// vpnPfd 标志无法区分这两者（v0.5.9 异步化后新增）。
//
//export Java_com_wails_app_WarpVpnService_nativeVpnRunning
func Java_com_wails_app_WarpVpnService_nativeVpnRunning(env *C.JNIEnv, obj C.jobject) C.jint {
	if androidVpnRunning() {
		return 1
	}
	return 0
}

// androidVpnLastError 返回最近一次 VPN/内核错误。
func androidVpnLastError() string {
	androidRuntime.mu.Lock()
	defer androidRuntime.mu.Unlock()
	return androidRuntime.lastErr
}

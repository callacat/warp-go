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

// protectSocket(int fd) → boolean：WarpVpnService 静态方法，对 TUN fd 之外的
// 关键 socket 调用 VpnService.protect()，豁免其 VPN 路由（Android 上应用自身
// 新 socket 也走 TUN，见 socketProtector）。返回 false = 服务未就绪/失败。
static jmethodID getProtectSocketMethod(JNIEnv* env, jclass cls) {
    return (*env)->GetStaticMethodID(env, cls, "protectSocket", "(I)Z");
}

// kernelFailed(String msg) → void：异步内核装配失败时通知 Java 自拆除
// （stopForeground + stopSelf + 关 TUN fd），避免"启动失败但通知栏残留 /
// 停止无响应"（v0.5.13 反馈"无法停止内核"）。
static jmethodID getKernelFailedMethod(JNIEnv* env, jclass cls) {
    return (*env)->GetStaticMethodID(env, cls, "kernelFailed", "(Ljava/lang/String;)V");
}

static jboolean callStaticBooleanMethod(JNIEnv* env, jclass cls, jmethodID mid, int arg) {
    return (*env)->CallStaticBooleanMethod(env, cls, mid, (jint)arg);
}

// exportDebugDiag() → String：把 debugdiag 数据打包到 MediaStore Downloads
// 并返回 URI（调试版）。非致命方法，未就绪时 androidExportDebugDiag 静默
// 跳过（不参与 ready 判定）。
static jmethodID getExportDiagMethod(JNIEnv* env, jclass cls) {
    return (*env)->GetStaticMethodID(env, cls, "exportDebugDiag", "()Ljava/lang/String;");
}

static jstring callStaticStringMethod(JNIEnv* env, jclass cls, jmethodID mid) {
    return (*env)->CallStaticObjectMethod(env, cls, mid);
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
	"fmt"
	"log"
	"sync"
	"time"
	_ "time/tzdata" // 内嵌时区数据库：Android 系统无 tzdata，不导入 LoadLocation 必失败
	"unsafe"

	"github.com/wailsapp/wails/v3/pkg/application"
	"golang.org/x/sys/unix"

	"warp/androidvpn"
	"warp/core"
	"warp/tunnel"
)

// androidDialTimeoutDefault 是 Android 内核装配的拨号总超时默认值。移动网络
// 下 QUIC/UDP 可能被运营商封锁，无限重试只会无限刷错误并让状态停在"连接中"；
// 超时后报明确错误，用户可检查网络后重试。可由 config.json 的
// dial_timeout_seconds（core.Config.DialTimeoutSeconds）覆盖：0 或缺失 = 默认
// 60s；正值 = 该秒数。
const androidDialTimeoutDefault = 60 * time.Second

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
	// startTime 是 VPN 内核装配成功（写入 androidRuntime.kernel）的时刻。
	// GUI GetStatus 的 Android 分支读它填充状态页"启动时间"——此前读
	// Server.kernel.StartTime（Android 上 Server 从未 Start → 零值，见
	// v0.5.22 修复"状态页启动时间不显示"）。
	startTime time.Time
	lastErr   string
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
	exportM      C.jmethodID // exportDebugDiag（调试版）；nil = 未缓存/无此方法
	ready        bool
}

// warpCtl 持有 WarpVpnService 的类引用与静态方法 ID（protectSocket /
// kernelFailed）。与 androidCtl 分开（那是 MainActivity）；在 nativeStartVpn
// 内于 Java 主线程缓存（nativeStartVpn 是 static native，JNI 传入的 obj
// 本身就是 WarpVpnService 的 jclass），供 startVpnKernel goroutine 里保护
// 拨号 socket 与上报装配失败时调用。
var warpCtl struct {
	mu          sync.Mutex
	cls         C.jclass // WarpVpnService 全局引用
	protectM    C.jmethodID
	kernelFailM C.jmethodID
	ready       bool
}

// Java_com_wails_app_WarpVpnService_nativeStartVpn 是 Java 侧
// WarpVpnService 的 JNI 入口：VpnService.Builder.establish() 拿到 TUN fd 后
// 传入，Go 侧装配 core.Kernel（MASQUE 隧道 + 分流引擎）与 androidvpn 栈并启动。
//
// 第二参数 dnsList 是 Java 在 establish() 前缓存的物理网络 DNS（逗号分隔
// IP 字符串，可为空串）——建立 VPN 后读会拿到 VPN 自身 DNS，时序无关解法：
// 提前缓存，注入 Config.PhysicalDNS 供 TUN DNS 拦截的国内域名分流使用
// （v0.5.30 阶段 12，见 design.md §3 防覆盖）。
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
func Java_com_wails_app_WarpVpnService_nativeStartVpn(env *C.JNIEnv, obj C.jobject, fd C.jint, dnsList C.jstring) C.jint {
	C.storeJvm(env)

	// 缓存 WarpVpnService 类引用 + protectSocket/kernelFailed 方法 ID。
	// 本函数在 Java 主线程（onStartCommand）执行——nativeStartVpn 是
	// static native，JNI 传入的第二参数 obj 就是 WarpVpnService 的
	// jclass（类对象），直接用它找静态方法即可。若延迟到 startVpnKernel
	// goroutine 里 FindClass 会错失应用 classloader（§6.8.3 教训）。
	// 注意：绝不能对 obj 再调 GetObjectClass——对 static native 方法
	// obj 已是类对象，再取类会得到 java.lang.Class，在其上找
	// protectSocket/kernelFailed 必然 NoSuchMethodError → 主线程抛异常
	// SIGABRT 闪退（v0.5.14 真机崩溃根因）。缓存失败不致命：拨号 socket
	// 无法 protect（连接失败路径变长）或装配失败无法通知 Java。
	clsRef := C.newGlobalRef(env, (C.jclass)(obj))
	if unsafe.Pointer(clsRef) != nil {
		protectM := C.getProtectSocketMethod(env, clsRef)
		kernelFailM := C.getKernelFailedMethod(env, clsRef)
		warpCtl.mu.Lock()
		warpCtl.cls = clsRef
		warpCtl.protectM = protectM
		warpCtl.kernelFailM = kernelFailM
		warpCtl.ready = unsafe.Pointer(protectM) != nil && unsafe.Pointer(kernelFailM) != nil
		warpCtl.mu.Unlock()
	}

	androidRuntime.mu.Lock()
	if androidRuntime.started {
		androidRuntime.mu.Unlock()
		log.Println("⚠ nativeStartVpn：VPN 已在运行")
		// Java 已 detachFd，fd 所有权在 Go：同步失败路径必须关闭，防泄漏。
		_ = unix.Close(int(fd))
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
		_ = unix.Close(int(fd))
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
		// Java 已 detachFd：同步失败路径 Go 负责关闭 fd（防泄漏）。
		_ = unix.Close(int(fd))
		return -1
	}

	// v0.5.30 阶段 12：Java 侧在 establish() 前缓存的物理网络 DNS（主来源）
	// 经 JNI 注入。优先级：Java 注入 > config.json 的 physical_dns
	// （androidconfig.go 已填）> 公共 DNS 兜底（NewDNSInterceptor）。空串/
	// 全非法时保留 config.json 值或兜底，不覆盖。
	if dnsList != nil {
		if cstr := C.jstringToChars(env, dnsList, nil); cstr != nil {
			parsed := parsePhysicalDNSCSV(C.GoString(cstr))
			C.releaseChars(env, dnsList, cstr)
			if len(parsed) > 0 {
				built.vpnCfg.PhysicalDNS = parsed
				log.Printf("✓ 物理 DNS 注入：%v", parsed)
			}
		}
	}

	// 先置 started + 创建装配取消信号：装配在 goroutine 异步进行，此标记
	// 表示"已受理"，闭合竞态——否则装配期间 androidVpnRunning() 返回 false，
	// 用户再点启动会再次触发（Service.Start 的幂等判断失效，产生双 Kernel）。
	// cancel 同时是装配取消信号：nativeStopVpn 在装配完成前到达时取消它，
	// startVpnKernel 每次装配前检查 ctx 已取消则中止（否则用户"启动后立刻
	// 停止"会得到装配照常完成、VPN 仍运行的反直觉结果）。
	// 拨号总超时：移动网络下 QUIC/UDP 可能被运营商封锁，无限指数退避重试
	// 只会无限刷"边缘不可达"并让状态永远停在"连接中"（v0.5.10 反馈）。
	// 默认 60s（androidDialTimeoutDefault），可由 config.json 的
	// dial_timeout_seconds 覆盖；0/缺失 = 默认 60s。
	dialTimeout := time.Duration(built.cfg.DialTimeoutSeconds) * time.Second
	if dialTimeout <= 0 {
		dialTimeout = androidDialTimeoutDefault
	}
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	androidRuntime.mu.Lock()
	androidRuntime.ctx = ctx
	androidRuntime.cancel = cancel
	androidRuntime.started = true
	androidRuntime.lastErr = ""
	androidRuntime.mu.Unlock()

	// 注册拨号 socket 保护器：Android 上 VpnService.establish() 后应用自身
	// 新 socket 也走 TUN，拨号 QUIC 的 ClientHello 会滞留未读取的 tun 里导致
	// 所有边缘超时（v0.5.13 反馈"连接所有边缘地址失败"）。protect() 豁免该
	// socket 走物理网络（根因修复，见 tunnel.socketProtector）。同时注册到
	// androidvpn：direct 直连（TCP/UDP）socket 同样需要豁免，否则环路风暴
	// （v0.5.17 模拟器实测 tun0 TX 31GB、浏览器不通）。
	tunnel.SetSocketProtector(androidProtectSocket)
	androidvpn.SetSocketProtector(androidProtectSocket)

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
	// debugdiag：启动时开启调试数据收集（release 构建为 no-op）。
	androidvpn.DebugSetDir(sandboxDir)
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
	built.vpnCfg.TunnelDNS = kernel.ResolveDNS // v0.5.24：TUN DNS 拦截 → 隧道内 DoH
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

	// runCancel 是装配完成后创建的运行期取消函数（见下方 ctx 切换）。声明在
	// rollback 之前以便闭包捕获：装配失败时它仍为 nil（只取消装配 ctx），
	// 装配成功后取消它即停止 kernel/vpn 生命周期。
	var runCancel context.CancelFunc

	// rollback 在 kernel/vpn 任一异步启动失败时拆除本实例并回滚状态。
	// 闭包捕获本地变量而非 androidRuntime 字段：若回滚前已有新实例
	// （started 被再次置 true），旧实例的拆除不碰新实例状态。
	rollback := func(name string, err error) {
		log.Printf("⚠ nativeStartVpn：%s 启动失败，回滚：%v", name, err)
		cancel()
		if runCancel != nil {
			runCancel()
		}
		if vpn != nil {
			_ = vpn.Stop()
		}
		if kernel != nil {
			_ = kernel.Close()
		}
		androidRuntime.mu.Lock()
		current := androidRuntime.kernel == kernel
		if current {
			androidRuntime.kernel = nil
			androidRuntime.vpn = nil
			androidRuntime.ctx = nil
			androidRuntime.cancel = nil
			androidRuntime.started = false
			androidRuntime.lastErr = err.Error()
		}
androidRuntime.mu.Unlock()
	if current {
		androidNotifyKernelFailed(name + "：" + err.Error())
	}
	// debugdiag：装配失败也收尾落盘（release 版为 no-op）。
	androidvpn.DebugStop()
	androidExportDebugDiag()
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

	// 装配完成后切换运行期 ctx：装配 ctx 带 60s 拨号超时（见 nativeStartVpn
	// 的 WithTimeout），若把它继续传给 kernel.Start/vpn.Start，超时到期时
	// sing-tun 栈随 ctx 取消整体关闭——用户看到"VPN 开"但 TUN 已死，且拨号
	// 耗时接近 60s 时（移动网络常态）栈只活几秒。运行期 ctx 改为从 background
	// 派生、生命周期只由 nativeStopVpn 的 cancel 控制，装配计时器不再约束
	// 运行。kernel.Start 内部对 ctx.Done 返回 nil（不触发 rollback），故用
	// runCancel 作为实例的停止信号，与 androidRuntime.cancel 保持一致。
	//
	// 校验 ctx 身份、写入运行状态、替换运行期 ctx 必须在同一临界区完成：
	// 拆分锁块会让 nativeStopVpn 在两次加锁之间插入——它读到 kernel 已写入
	// 但 ctx 仍是装配 ctx，清空状态后本函数又写入 runCtx，复活已停止的实例
	// 且 runCancel 泄漏（无人再取消它）。
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
	runCtx, runCancel := context.WithCancel(context.Background())
	androidRuntime.kernel = kernel
	androidRuntime.vpn = vpn
	androidRuntime.ctx = runCtx
	androidRuntime.cancel = runCancel
	if androidRuntime.startTime.IsZero() {
		androidRuntime.startTime = time.Now()
	}
	androidRuntime.mu.Unlock()

	// 异步启动 kernel/vpn。recover 兜底（v0.5.16 教训）：sing 库会直接
	// panic（如 udpnat.New 对 timeout==0），goroutine 内 panic 将 SIGABRT
	// 拖垮进程——recover 后走 failStart 正常回滚，而不是崩溃。
	go func() {
		defer func() {
			if r := recover(); r != nil {
				rollback("kernel panic", fmt.Errorf("panic: %v", r))
			}
		}()
		if err := kernel.Start(runCtx); err != nil {
			rollback("kernel", err)
		}
	}()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				rollback("TUN 栈 panic", fmt.Errorf("panic: %v", r))
			}
		}()
		if err := vpn.Start(runCtx); err != nil {
			rollback("TUN 栈", err)
		}
	}()

	log.Printf("✓ VPN 已启动（fd=%d）", fd)
}

// failStart 记录异步装配失败并回滚状态：清 started（允许用户重试）、写
// lastErr（GetStatus 展示），并通知 Java 侧自拆除（release TUN + 停前台
// 服务）。此前异步失败只改 Go 状态，Java 的 vpnPfd/nativeRunning 保持 true、
// 前台通知残留——用户点停止看似无响应、通知栏一直挂着（v0.5.13 反馈
// "无法停止内核"）。Java 收到 kernelFailed 后自停，停止按钮随后总是幂等生效。
func failStart(stage string, err error) {
	androidRuntime.mu.Lock()
	androidRuntime.started = false
	androidRuntime.lastErr = err.Error()
	androidRuntime.mu.Unlock()
	log.Printf("⚠ VPN 启动失败（%s）：%v", stage, err)
	androidNotifyKernelFailed(stage + "：" + err.Error())
}

// failStartCtx 区分装配 ctx 的两种中止原因并上报：
//   - DeadlineExceeded → 拨号总超时（默认 60s，见 androidDialTimeoutDefault）
//     ——边缘不可达被运营商封锁/弱网，报明确错误供用户检查网络后重试，而非
//     无限重试（v0.5.10 反馈"一直边缘不可达 3.2s 后重试"）；
//   - 其余（Canceled）→ 用户装配中点了停止，正常中止。
func failStartCtx(ctx context.Context, err error) {
	if ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		failStart("连接边缘超时", errors.New("连接 WARP 边缘超时，请检查网络后重试"))
		return
	}
	failStart("已取消", err)
}

// androidNotifyKernelFailed 通知 Java 侧内核装配失败：经 warpCtl 缓存的
// kernelFailed 静态方法触发 WarpVpnService 自拆除（release TUN + 停前台 +
// stopSelf），使启动失败后通知栏立即消失、停止按钮幂等可用。桥未就绪（类/
// 方法 ID 未缓存）时仅记日志——此时 Java 侧仍持有 TUN，靠 onStartCommand
// 的 stale-TUN 重入守卫在下次启动时释放。
func androidNotifyKernelFailed(msg string) {
	warpCtl.mu.Lock()
	cls, mid, ready := warpCtl.cls, warpCtl.kernelFailM, warpCtl.ready
	warpCtl.mu.Unlock()
	if !ready || unsafe.Pointer(cls) == nil || unsafe.Pointer(mid) == nil {
		log.Println("⚠ kernelFailed 桥未就绪，无法通知 Java 自拆除")
		return
	}
	var needsDetach C.int
	env := C.getEnv(&needsDetach)
	if env == nil {
		log.Println("⚠ kernelFailed：无法获取 JNIEnv")
		return
	}
	defer C.releaseEnv(needsDetach)
	cstr := C.CString(msg)
	C.callStaticVoidMethodStr(env, cls, mid, cstr)
	C.free(unsafe.Pointer(cstr))
}

// androidProtectSocket 是 tunnel.socketProtector 的 Android 实现：调用
// WarpVpnService.protectSocket(fd)（Java 侧 sInstance.protect(fd)），把拨号
// socket 豁免出 VPN 路由走物理网络。桥未就绪时返回错误（dialAddr 记日志后
// 继续，连接仍会失败但原因可排查）。
func androidProtectSocket(fd int) error {
	warpCtl.mu.Lock()
	cls, mid, ready := warpCtl.cls, warpCtl.protectM, warpCtl.ready
	warpCtl.mu.Unlock()
	if !ready || unsafe.Pointer(cls) == nil || unsafe.Pointer(mid) == nil {
		return errors.New("WarpVpnService protectSocket 桥未就绪")
	}
	var needsDetach C.int
	env := C.getEnv(&needsDetach)
	if env == nil {
		return errors.New("无法获取 JNIEnv")
	}
	defer C.releaseEnv(needsDetach)
	ok := C.callStaticBooleanMethod(env, cls, mid, C.jint(fd)) != 0
	if !ok {
		return errors.New("protectSocket 返回 false（服务未就绪）")
	}
	return nil
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
	// debugdiag：停止时收尾落盘并触发 Java 侧导出到 Download（调试版）。
	androidvpn.DebugStop()
	androidExportDebugDiag()
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
	exportM := C.getExportDiagMethod(env, clsRef)
	if unsafe.Pointer(startM) == nil || unsafe.Pointer(stopM) == nil || unsafe.Pointer(openBrowserM) == nil {
		log.Println("⚠ nativeBridgeReady：找不到 requestStartVpn/requestStopVpn/openExternalBrowser 静态方法")
		return -1
	}
	androidCtl.mu.Lock()
	androidCtl.cls = clsRef
	androidCtl.startM = startM
	androidCtl.stopM = stopM
	androidCtl.openBrowserM = openBrowserM
	androidCtl.exportM = exportM
	androidCtl.ready = true
	androidCtl.mu.Unlock()
	log.Println("✓ Android 反向 JNI 桥就绪（requestStartVpn/requestStopVpn/openExternalBrowser）")
	return 0
}

// androidExportDebugDiag 请求 Java 侧把 debugdiag 数据打包到 MediaStore
// Downloads 并返回 content URI（调试版）。release 构建无 exportDebugDiag 方
// 法 → 静默跳过。VPN 停止/装配失败时调用，URI 打到 GUI 日志页供用户取文件。
func androidExportDebugDiag() string {
	androidCtl.mu.Lock()
	cls, mid, ready := androidCtl.cls, androidCtl.exportM, androidCtl.ready
	androidCtl.mu.Unlock()
	if !ready || unsafe.Pointer(cls) == nil || unsafe.Pointer(mid) == nil {
		return ""
	}
	var needsDetach C.int
	env := C.getEnv(&needsDetach)
	if env == nil {
		return ""
	}
	defer C.releaseEnv(needsDetach)
	js := C.callStaticStringMethod(env, cls, mid)
	if unsafe.Pointer(js) == nil {
		return ""
	}
	chars := C.jstringToChars(env, js, nil)
	if chars == nil {
		return ""
	}
defer C.releaseChars(env, js, chars)
	uri := C.GoString(chars)
	if uri != "" {
		log.Printf("✓ 调试数据已导出：%s", uri)
	}
	return uri
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
		// 桥未就绪时打日志（与启动路径一致），避免"点停止没反应且无日志"
		// 掩埋停止链路问题（v0.5.18 真机停止排查）。不再静默 no-op。
		err := errors.New("Android VPN 桥未就绪（MainActivity 未初始化），停止未执行")
		log.Printf("⚠ 停止失败：%v", err)
		return err
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

// androidVpnStartTime 返回 VPN 内核装配成功的时间（零值 = 从未运行）。
func androidVpnStartTime() time.Time {
	androidRuntime.mu.Lock()
	defer androidRuntime.mu.Unlock()
	return androidRuntime.startTime
}

// androidVpnKernel 返回当前运行的 VPN 内核（nil = 未运行）。GUI GetStatus
// 的 Android 分支从它读真实分流统计（Stats）与规则数，而非从未启动的
// Server.kernel（nil → 全 0，v0.5.22 修复"流量统计无变化"）。
func androidVpnKernel() *core.Kernel {
	androidRuntime.mu.Lock()
	defer androidRuntime.mu.Unlock()
	return androidRuntime.kernel
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

// androidReloadRules 从磁盘重新加载路由规则（规则页"重新加载"按钮）。
// Android 上分流引擎挂在 androidRuntime.kernel（VpnService 驱动的
// core.Kernel），而非 core.Server.kernel（SOCKS 内核在 Android 永不启动）。
// VPN 未运行 / 引擎未就绪时返回明确错误（此前 Service.ReloadRules 走
// Server.ReloadRules → s.kernel==nil → "分流引擎未初始化"，规则页点击报错
// 且无法生效——v0.5.12 真机反馈）。
func androidReloadRules() error {
	androidRuntime.mu.Lock()
	k := androidRuntime.kernel
	androidRuntime.mu.Unlock()
	if k == nil {
		return errors.New("分流引擎未初始化（请先启动 VPN）")
	}
	return k.ReloadRules()
}

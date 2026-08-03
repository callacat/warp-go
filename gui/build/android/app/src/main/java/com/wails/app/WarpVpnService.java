package com.wails.app;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.content.Context;
import android.content.Intent;
import android.content.pm.ServiceInfo;
import android.net.VpnService;
import android.os.Build;
import android.os.IBinder;
import android.os.ParcelFileDescriptor;
import android.util.Log;

import androidx.annotation.Nullable;
import androidx.core.app.NotificationCompat;

import java.io.IOException;
import java.net.Inet4Address;
import java.net.Inet6Address;
import java.net.InetAddress;

/**
 * WarpVpnService owns the Android TUN device and hands its file descriptor to
 * the Go native library ({@code gui/androidbridge.go}) via JNI:
 *
 * <pre>
 *   Java_com_wails_app_WarpVpnService_nativeStartVpn(int fd) -> 0 ok / -1 fail
 *   Java_com_wails_app_WarpVpnService_nativeStopVpn()        -> idempotent
 * </pre>
 *
 * The Go side runs the MASQUE/QUIC tunnel over the TUN fd and routes the
 * resulting packets back through the Android network stack. The service is
 * started from MainActivity (after the VpnService consent flow) with the IPs
 * WARP assigned to the device passed as extras
 * ({@link #EXTRA_ASSIGNED_IPV4} / {@link #EXTRA_ASSIGNED_IPV6}); the extras
 * may be absent, in which case the tunnel is still established with catch-all
 * routes only.
 */
public class WarpVpnService extends VpnService {
    private static final String TAG = "warp-go";

    /** Extra: IPv4 address assigned to this device, string, may be absent. */
    public static final String EXTRA_ASSIGNED_IPV4 = "assigned_ipv4";

    /** Extra: IPv6 address assigned to this device, string, may be absent. */
    public static final String EXTRA_ASSIGNED_IPV6 = "assigned_ipv6";

    // Reuse the channel WailsForegroundService already creates:
    // createNotificationChannel is idempotent, so no duplicate channel is
    // produced when both services run in the same process.
    private static final String CHANNEL_ID = "wails_foreground";

    // Distinct from WailsForegroundService.NOTIFICATION_ID (0x57A1) so the
    // two foreground notifications can coexist.
    private static final int NOTIFICATION_ID = 0x57A2; // "WAR"

    // Native methods implemented in Go (gui/androidbridge.go). JNI resolves
    // them as Java_com_wails_app_WarpVpnService_nativeStartVpn /
    // Java_com_wails_app_WarpVpnService_nativeStopVpn / nativeVpnRunning.
    private static native int nativeStartVpn(int fd);
    private static native int nativeStopVpn();
    private static native int nativeVpnRunning();

    static {
        // Same .so as WailsBridge: it carries both Wails' exports and ours.
        // Loading twice in one process is harmless, and this guarantees the
        // library is present even if the service starts standalone.
        System.loadLibrary("wails");
    }

    private volatile ParcelFileDescriptor vpnPfd;
    private volatile boolean nativeRunning = false;
    private static volatile WarpVpnService sInstance;

    /**
     * Exempt a socket from VPN routing by protecting it, so its traffic uses
     * the physical network instead of the TUN. Once establish() installs
     * catch-all routes, the app's OWN new sockets also route through the TUN —
     * and the TUN isn't read until after the Go dial succeeds, so the QUIC
     * ClientHello would sit unprocessed in the tun and every edge handshake
     * times out ("所有边缘地址均失败"). Go calls this (via JNI) on the edge
     * dial socket. Returns false when the service instance isn't live yet.
     */
    public static boolean protectSocket(int fd) {
        WarpVpnService s = sInstance;
        if (s == null) return false;
        try {
            return s.protect(fd);
        } catch (Exception e) {
            Log.e(TAG, "protect(" + fd + ") failed", e);
            return false;
        }
    }

    /**
     * Asynchronous kernel-assembly failure notification from Go: the Go side
     * accepted the start (returned 0) but the dial/assembly later failed, so
     * this service must tear itself down (release the TUN fd, remove the
     * foreground state, stop). Without this the notification and the native
     * state linger after a failed start — the "无法停止内核" symptom. Safe to
     * call from any thread: stopForeground/stopSelf are thread-safe.
     */
    public static void kernelFailed(String msg) {
        MainActivity.nativeLogMessage("error", "内核启动失败：" + msg);
        WarpVpnService s = sInstance;
        if (s == null) return;
        Log.e(TAG, "kernel assembly failed, tearing down: " + msg);
        s.stopForeground(STOP_FOREGROUND_REMOVE);
        s.stopSelf();
        s.closeNative();
    }

    @Override
    public void onCreate() {
        super.onCreate();
        sInstance = this;
    }

    /**
     * Stop the VPN service from the UI thread. Plain {@code stopService}
     * does not stop a foreground service (Android 8+): the service stays
     * foreground and {@code onDestroy} never fires — the reported v0.5.10
     * "停止按钮无效" root cause. Must remove the foreground state first.
     */
    public static void stop(Context ctx) {
        WarpVpnService svc = sInstance;
        if (svc != null) {
            svc.stopForeground(STOP_FOREGROUND_REMOVE);
            svc.stopSelf();
        } else {
            ctx.stopService(new Intent(ctx, WarpVpnService.class));
        }
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        if (vpnPfd != null || nativeRunning) {
            // 内核真在运行 → 幂等跳过（START_STICKY 重投/并发 start）。
            // 仅 Java 本地标志非空但 Go 侧已失败（异步装配 failed，
            // started=false）→ 释放旧 TUN fd 重新建立，否则用户重试
            // 会被"已运行"守卫拦截，表现为点了没反应（v0.5.9）。
            if (nativeVpnRunning() != 0) {
                return START_STICKY;
            }
            Log.w(TAG, "native kernel not running but state held - releasing stale TUN and re-establishing");
            closeNative();
        }

        startForeground();

        String ipv4 = intent != null ? intent.getStringExtra(EXTRA_ASSIGNED_IPV4) : null;
        String ipv6 = intent != null ? intent.getStringExtra(EXTRA_ASSIGNED_IPV6) : null;
        // startVpnService 不传 extras（MainActivity 不解析 reg.json），从
        // 应用沙箱 reg.json 读取分配的隧道地址兜底——否则 VpnService.Builder
        // 无地址，establish() 抛 "At least one address must be specified"
        // （v0.5.8 真机反馈"VPN 建立失败"）。
        if (ipv4 == null || ipv6 == null) {
            String[] assigned = readAssignedAddrs();
            if (ipv4 == null) ipv4 = assigned[0];
            if (ipv6 == null) ipv6 = assigned[1];
        }

        VpnService.Builder builder = new VpnService.Builder();
        builder.setSession("warp-go");
        builder.setMtu(1500);
        builder.setBlocking(true);
        builder.addRoute("0.0.0.0", 0);
        builder.addRoute("::", 0);

        addAddress(builder, ipv4);
        addAddress(builder, ipv6);

        ParcelFileDescriptor pfd;
        try {
            pfd = builder.establish();
        } catch (IllegalArgumentException | SecurityException e) {
            Log.e(TAG, "establish() failed", e);
            MainActivity.nativeLogMessage("error", "VPN 建立失败：" + e.getMessage());
            stopSelf();
            return START_NOT_STICKY;
        }
        if (pfd == null) {
            // The user revoked the VPN permission (or a concurrent prepare()
            // consumed it) between prepare and establish.
            Log.e(TAG, "establish() returned null - VPN permission not granted");
            MainActivity.nativeLogMessage("error", "VPN 授权已失效（可能已在系统设置中关闭），请重新授权后启动");
            stopSelf();
            return START_NOT_STICKY;
        }

        // 置位必须在 nativeStartVpn 之前：Start 现在异步受理（返回 0 即已
        // 受理，内核在 goroutine 内装配），若置位在 return 后，onStartCommand
        // 重入/START_STICKY 复活会以为未启动而再 establish + 再 nativeStartVpn。
        vpnPfd = pfd;
        nativeRunning = true;

        int fd = pfd.getFd();
        Log.i(TAG, "TUN established, handing fd=" + fd + " to nativeStartVpn");
        MainActivity.nativeLogMessage("info", "VPN 隧道已建立（fd=" + fd + "），正在启动内核...");
        int result;
        try {
            result = nativeStartVpn(fd);
        } catch (Throwable t) {
            Log.e(TAG, "nativeStartVpn threw", t);
            result = -1;
        }
        if (result != 0) {
            Log.e(TAG, "nativeStartVpn returned " + result + ", tearing down");
            MainActivity.nativeLogMessage("error", "内核启动失败（错误码 " + result + "），请查看日志页");
            closeNative();
            stopSelf();
            return START_NOT_STICKY;
        }

        Log.i(TAG, "VPN accepted async start (fd=" + fd + ")");
        MainActivity.nativeLogMessage("info", "✓ VPN 已受理启动（fd=" + fd + "）");
        return START_STICKY;
    }

    /**
     * Close the TUN descriptor and reset the run-state flags. Used when
     * nativeStartVpn rejects synchronously (result != 0) or on teardown.
     * Idempotent: safe to call more than once.
     */
    private void closeNative() {
        ParcelFileDescriptor pfd = vpnPfd;
        vpnPfd = null;
        nativeRunning = false;
        if (pfd != null) {
            closePfd(pfd);
        }
    }

    /**
     * Add an assigned address (if present and parseable) to the builder.
     * IPv4 addresses are added with prefix 32, IPv6 with prefix 128. The
     * unspecified/any-local addresses are skipped - they are not assignable.
     */
    private void addAddress(VpnService.Builder builder, String ip) {
        if (ip == null || ip.isEmpty()) {
            return;
        }
        try {
            InetAddress addr = InetAddress.getByName(ip);
            if (addr.isAnyLocalAddress()) {
                Log.w(TAG, "skipping any-local address: " + ip);
                return;
            }
            boolean isV6 = addr instanceof Inet6Address; // Inet4Address/Inet6Address are siblings
            builder.addAddress(addr, isV6 ? 128 : 32);
            Log.i(TAG, "assigned " + (isV6 ? "IPv6" : "IPv4") + " " + ip);
        } catch (Exception e) {
            Log.w(TAG, "invalid assigned IP: " + ip, e);
        }
    }

    /**
     * Read assigned_ipv4 / assigned_ipv6 from the sandbox reg.json
     * ({@code getFilesDir()/reg.json}) as a fallback when the start intent
     * carries no extras. Returns a 2-element array [ipv4, ipv6]; missing or
     * unparseable values are empty strings.
     */
    private String[] readAssignedAddrs() {
        String[] out = { "", "" };
        try {
            java.io.File reg = new java.io.File(getFilesDir(), "reg.json");
            if (!reg.exists()) {
                return out;
            }
            java.io.InputStream in = new java.io.FileInputStream(reg);
            try {
                byte[] buf = new byte[(int) reg.length()];
                int off = 0;
                while (off < buf.length) {
                    int n = in.read(buf, off, buf.length - off);
                    if (n < 0) break;
                    off += n;
                }
                org.json.JSONObject o = new org.json.JSONObject(new String(buf, 0, off, "UTF-8"));
                out[0] = o.optString("assigned_ipv4", "");
                out[1] = o.optString("assigned_ipv6", "");
            } finally {
                in.close();
            }
        } catch (Exception e) {
            Log.w(TAG, "readAssignedAddrs failed", e);
        }
        return out;
    }

    @Override
    public void onDestroy() {
        Log.i(TAG, "onDestroy - stopping VPN");
        MainActivity.nativeLogMessage("info", "正在停止 VPN...");
        stopNativeAndClose();
        sInstance = null;
        super.onDestroy();
    }

    @Override
    public void onRevoke() {
        // The user revoked the VPN permission; the system stops the service
        // after this, so stop the tunnel and release the TUN now.
        Log.i(TAG, "onRevoke - VPN permission revoked");
        MainActivity.nativeLogMessage("error", "VPN 授权被撤销（系统设置中关闭），隧道已停止");
        stopNativeAndClose();
        stopSelf();
    }

    /**
     * Idempotent teardown: stops the Go tunnel (no-op if never started) and
     * closes the TUN fd. Safe to run more than once - onDestroy may follow
     * onRevoke, and both must not double-close the ParcelFileDescriptor.
     */
    private void stopNativeAndClose() {
        if (nativeRunning) {
            try {
                nativeStopVpn();
            } catch (Throwable t) {
                Log.e(TAG, "nativeStopVpn threw", t);
            } finally {
                nativeRunning = false;
            }
        }
        closeNative();
    }

    private void closePfd(ParcelFileDescriptor pfd) {
        try {
            pfd.close();
        } catch (IOException e) {
            Log.w(TAG, "close failed", e);
        }
    }

    private void startForeground() {
        Notification n = buildNotification();
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            startForeground(NOTIFICATION_ID, n, ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC);
        } else {
            startForeground(NOTIFICATION_ID, n);
        }
    }

    private Notification buildNotification() {
        NotificationManager nm = (NotificationManager) getSystemService(NOTIFICATION_SERVICE);
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            NotificationChannel ch = new NotificationChannel(
                    CHANNEL_ID, "warp-go VPN", NotificationManager.IMPORTANCE_LOW);
            nm.createNotificationChannel(ch);
        }

        PendingIntent contentIntent = null;
        Intent launch = getPackageManager().getLaunchIntentForPackage(getPackageName());
        if (launch != null) {
            int piFlags = Build.VERSION.SDK_INT >= Build.VERSION_CODES.M
                    ? PendingIntent.FLAG_IMMUTABLE : 0;
            contentIntent = PendingIntent.getActivity(this, 0, launch, piFlags);
        }

        return new NotificationCompat.Builder(this, CHANNEL_ID)
                .setSmallIcon(android.R.drawable.ic_popup_sync)
                .setContentTitle("warp-go")
                .setContentText("Secure tunnel active")
                .setOngoing(true)
                .setContentIntent(contentIntent)
                .build();
    }

    @Nullable
    @Override
    public IBinder onBind(Intent intent) {
        return null;
    }
}

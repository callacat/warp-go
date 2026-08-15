import { useEffect, useState } from "react";
import { KeyRound, Play, Square, Globe, Trash2 } from "lucide-react";
import {
  deregister,
  getStatus,
  register,
  setSystemProxy,
  start,
  stop,
} from "../lib/api";
import { fromConfig, AppConfig, AppStatus } from "../lib/types";
import { Card, Button, Toggle, StatusPill } from "../components/ui";
import { usePoll } from "../lib/usePoll";
import { useAsyncAction } from "../lib/useAsyncAction";

export default function StatusPage() {
  const [refreshKey, setRefreshKey] = useState(0);
  const { data: statusRaw } = usePoll(getStatus, 2000, [refreshKey]);
  const { data: configRaw } = usePoll(getConfigOnce, 5000);
  const config: AppConfig = fromConfig(configRaw);
  // getStatus 已返回 fromStatus 归一化后的 AppStatus（camelCase），不要再
  // 包一层 fromStatus——二次归一化会把 initDone/isAndroid 读成 undefined
  // （它们只认 init_done/is_android 的 snake_case），导致"初始化完成但按钮
  // 灰"和"Android 系统代理卡片不隐藏"（v0.5.7 真机反馈）。首次加载前为
  // null，用兜底值避免渲染期空指针。
  const status: AppStatus = statusRaw ?? {
    running: false,
    listening: "127.0.0.1:40000",
    registered: false,
    isAndroid: false,
    initDone: false,
    sysProxyOn: false,
    counters: { proxy: 0, direct: 0, miss: 0, rejected: 0 },
    registration: null,
  };

  const { busy, error: actionError, notice, run } = useAsyncAction();
  const [confirmDeregister, setConfirmDeregister] = useState(false);
  const [confirmTimer, setConfirmTimer] = useState<ReturnType<typeof setTimeout> | null>(null);
  // proxyEnabled 跟随轮询的 status.sysProxyOn（后端每 2s 读真实系统状态）：
  // 外部软件关闭系统代理时开关自动变关（v0.5.7 反馈"其它软件关闭时 GUI
  // 应跟随"）。初始化时读一次兜底（首帧 bridge 未就绪时 status 为 null）。
  const [proxyEnabled, setProxyEnabled] = useState(false);

  useEffect(() => {
    if (statusRaw) setProxyEnabled(statusRaw.sysProxyOn);
    else getSystemProxyOnce().then(setProxyEnabled);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [statusRaw]);

  // 卸载时清理注销确认定时器，避免泄漏。
  useEffect(() => () => {
    if (confirmTimer) clearTimeout(confirmTimer);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const toggleRunning = async () => {
    await run(status.running ? "stop" : "start", async () => {
      if (status.running) await stop();
      else await start();
    });
  };

  const toggleProxy = async (v: boolean) => {
    setProxyEnabled(v);
    try {
      await setSystemProxy(v);
    } catch (e) {
      // run 不适合这里（需要回滚 toggle），手写错误处理
      run("proxy", async () => { throw e; });
      setProxyEnabled(!v);
    }
  };

  const onRegister = async () => {
    const res = await run("register", async () => {
      const r = await register();
      return r;
    }, undefined);
    if (res) {
      if (res.existing) run("register", async () => {}, "已存在注册，无需重复操作");
      else run("register", async () => {}, `注册成功（id=${res.id}）`);
      setRefreshKey(k => k + 1);
    }
  };

  const onDeregister = async () => {
    // Android WebView 不支持 window.confirm（静默返回 false → 无动作）。
    // 用自绘两段确认：第一次点"注销"进入确认态，再点一次才真正执行；
    // 5 秒无操作或点其它按钮自动取消。
    if (!confirmDeregister) {
      setConfirmDeregister(true);
      setConfirmTimer(setTimeout(() => setConfirmDeregister(false), 5000));
      return;
    }
    clearTimeout(confirmTimer ?? undefined);
    setConfirmDeregister(false);
    await run("deregister", async () => {
      await deregister();
    }, "已注销：本地注册信息已删除");
    setRefreshKey(k => k + 1);
  };

  return (
    <div className="space-y-4">
      {notice && (
        <div className="rounded-lg bg-emerald-50 px-4 py-3 text-sm text-emerald-700 dark:bg-emerald-950/40 dark:text-emerald-300">
          {notice}
        </div>
      )}
      {!status.registered && (
        <div className="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-amber-300 bg-amber-50 px-4 py-3 dark:border-amber-700/60 dark:bg-amber-950/40">
          <div>
            <p className="text-sm font-medium text-amber-800 dark:text-amber-200">
              尚未注册 WARP
            </p>
            <p className="text-xs text-amber-700/80 dark:text-amber-300/70">
              首次使用需注册（创建 Cloudflare WARP 账号）后才能启动代理。
            </p>
          </div>
          <Button
            onClick={onRegister}
            loading={busy === "register"}
            variant="secondary"
            className="shrink-0"
          >
            <KeyRound className="h-4 w-4" /> 一键注册
          </Button>
        </div>
      )}

      <Card
        title="运行状态"
        action={<StatusPill ok={status.running} text={status.running ? "运行中" : "已停止"} />}
      >
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
          <div>
            <p className="text-xs text-slate-500 dark:text-slate-400">监听地址</p>
            <p className="mt-1 font-mono text-sm text-slate-900 dark:text-slate-100">
              {status.listening}
            </p>
          </div>
          <div>
            <p className="text-xs text-slate-500 dark:text-slate-400">启动时间</p>
            <p className="mt-1 font-mono text-sm text-slate-900 dark:text-slate-100">
              {status.startedAt ?? "—"}
            </p>
          </div>
          <div>
            <p className="text-xs text-slate-500 dark:text-slate-400">规则文件</p>
            <p className="mt-1 break-all font-mono text-sm text-slate-900 dark:text-slate-100">
              {config.rulesPath}
            </p>
          </div>
        </div>
        {status.error && (
          <p className="mt-4 rounded-lg bg-red-50 px-3 py-2 text-sm text-red-700 dark:bg-red-950/40 dark:text-red-300">
            {status.error}
          </p>
        )}
        <div className="mt-5 flex flex-wrap items-center gap-3">
          <Button
            onClick={toggleRunning}
            variant={status.running ? "danger" : "primary"}
            loading={busy !== null}
            disabled={!status.running && !status.initDone}
          >
            {status.running ? (
              <>
                <Square className="h-4 w-4" /> 停止
              </>
            ) : (
              <>
                <Play className="h-4 w-4" /> 启动
              </>
            )}
          </Button>
          {!status.initDone && !status.running && (
            <span className="text-xs text-amber-600 dark:text-amber-400">
              正在初始化（默认规则 / GEO 数据库下载中），完成后即可启动
            </span>
          )}
          <Button onClick={onDeregister} loading={busy === "deregister"} variant="danger">
            <Trash2 className="h-4 w-4" />
            {confirmDeregister ? "确认注销？（再次点击执行）" : "注销（-del）"}
          </Button>
          {confirmDeregister && (
            <span className="text-xs text-red-600 dark:text-red-400">
              注销将删除本地注册信息并通知服务器，5 秒无操作自动取消
            </span>
          )}
          {actionError && (
            <span className="text-sm text-red-600 dark:text-red-400">{actionError}</span>
          )}
        </div>
      </Card>

      {status.registration && (
        <Card
          title="注册信息"
        >
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
            <div>
              <p className="text-xs text-slate-500 dark:text-slate-400">设备 ID</p>
              <p className="mt-1 break-all font-mono text-xs text-slate-900 dark:text-slate-100">
                {status.registration.id || "—"}
              </p>
            </div>
            <div>
              <p className="text-xs text-slate-500 dark:text-slate-400">账号</p>
              <p className="mt-1 break-all font-mono text-xs text-slate-900 dark:text-slate-100">
                {status.registration.account || "—"}
              </p>
            </div>
            <div>
              <p className="text-xs text-slate-500 dark:text-slate-400">密钥类型</p>
              <p className="mt-1 font-mono text-sm text-slate-900 dark:text-slate-100">
                {status.registration.keyType || "—"}
              </p>
            </div>
            <div>
              <p className="text-xs text-slate-500 dark:text-slate-400">分配的 IPv4</p>
              <p className="mt-1 font-mono text-sm text-emerald-600 dark:text-emerald-400">
                {status.registration.assignedIPv4 || "—"}
              </p>
            </div>
            <div>
              <p className="text-xs text-slate-500 dark:text-slate-400">分配的 IPv6</p>
              <p className="mt-1 font-mono text-sm text-emerald-600 dark:text-emerald-400">
                {status.registration.assignedIPv6 || "—"}
              </p>
            </div>
            <div>
              <p className="text-xs text-slate-500 dark:text-slate-400">边缘 IPv4</p>
              <p className="mt-1 font-mono text-sm text-slate-900 dark:text-slate-100">
                {status.registration.endpointV4 || "—"}
              </p>
            </div>
            <div>
              <p className="text-xs text-slate-500 dark:text-slate-400">边缘 IPv6</p>
              <p className="mt-1 font-mono text-sm text-slate-900 dark:text-slate-100">
                {status.registration.endpointV6 || "—"}
              </p>
            </div>
            <div>
              <p className="text-xs text-slate-500 dark:text-slate-400">边缘端口</p>
              <p className="mt-1 font-mono text-sm text-slate-900 dark:text-slate-100">
                {status.registration.endpointPorts?.length
                  ? status.registration.endpointPorts.join(", ")
                  : "—"}
              </p>
            </div>
            <div>
              <p className="text-xs text-slate-500 dark:text-slate-400">隧道类型</p>
              <p className="mt-1 font-mono text-sm text-slate-900 dark:text-slate-100">
                {status.registration.tunnelType || "masque"}
              </p>
            </div>
          </div>
        </Card>
      )}

      <Card title="流量统计">
        <div className="grid grid-cols-2 gap-4 lg:grid-cols-4">
          <div className="rounded-lg bg-orange-50 p-4 dark:bg-orange-950/30">
            <p className="text-xs text-orange-700 dark:text-orange-300">走隧道（proxy）</p>
            <p className="mt-1 text-2xl font-semibold text-orange-700 dark:text-orange-300">
              {status.counters.proxy}
            </p>
          </div>
          <div className="rounded-lg bg-emerald-50 p-4 dark:bg-emerald-950/30">
            <p className="text-xs text-emerald-700 dark:text-emerald-300">直连（direct）</p>
            <p className="mt-1 text-2xl font-semibold text-emerald-700 dark:text-emerald-300">
              {status.counters.direct}
            </p>
          </div>
          <div className="rounded-lg bg-slate-100 p-4 dark:bg-slate-800">
            <p className="text-xs text-slate-600 dark:text-slate-400">未命中（miss）</p>
            <p className="mt-1 text-2xl font-semibold text-slate-700 dark:text-slate-300">
              {status.counters.miss}
            </p>
          </div>
          <div className="rounded-lg bg-red-50 p-4 dark:bg-red-950/30">
            <p className="text-xs text-red-700 dark:text-red-300">拦截（reject）</p>
            <p className="mt-1 text-2xl font-semibold text-red-700 dark:text-red-300">
              {status.counters.rejected}
            </p>
          </div>
        </div>
      </Card>

      {!status.isAndroid && (
        <Card title="系统代理">
          <div className="flex items-center justify-between gap-4">
            <div className="flex items-center gap-3">
              <Globe className="h-5 w-5 text-slate-400" />
              <div>
                <p className="text-sm font-medium text-slate-800 dark:text-slate-200">
                  Windows / macOS / Linux 系统代理
                </p>
                <p className="text-xs text-slate-500 dark:text-slate-400">
                  将系统 HTTP/SOCKS 代理指向 {status.listening}
                </p>
              </div>
            </div>
            <Toggle checked={proxyEnabled} onChange={toggleProxy} label="系统代理" />
          </div>
        </Card>
      )}
    </div>
  );
}

// Small local helpers to avoid importing the whole api surface twice.
import { getConfig, getSystemProxyEnabled } from "../lib/api";
async function getConfigOnce(): Promise<AppConfig | null> {
  try {
    return await getConfig();
  } catch {
    return null;
  }
}
async function getSystemProxyOnce(): Promise<boolean> {
  try {
    return await getSystemProxyEnabled();
  } catch {
    return false;
  }
}

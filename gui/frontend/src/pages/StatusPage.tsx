import { useEffect, useState } from "react";
import { KeyRound, Play, Square, Globe, Trash2, Radar } from "lucide-react";
import {
  deregister,
  getStatus,
  register,
  scanEdges,
  setSystemProxy,
  start,
  stop,
} from "../lib/api";
import { fromStatus, fromConfig, AppStatus, AppConfig } from "../lib/types";
import { Card, Button, Toggle, StatusPill } from "../components/ui";

function usePoll<T>(fn: () => Promise<T>, ms: number, deps: unknown[] = []) {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    let alive = true;
    const tick = async () => {
      try {
        const v = await fn();
        if (alive) {
          setData(v);
          setError(null);
        }
      } catch (e) {
        if (alive) setError(String(e));
      }
    };
    void tick();
    const id = setInterval(tick, ms);
    return () => {
      alive = false;
      clearInterval(id);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);
  return { data, error };
}

export default function StatusPage() {
  const { data: statusRaw } = usePoll(getStatus, 2000);
  const status: AppStatus = fromStatus(statusRaw);
  const { data: configRaw } = usePoll(getConfigOnce, 5000);
  const config: AppConfig = fromConfig(configRaw);

  const [busy, setBusy] = useState<string | null>(null);
  const [proxyEnabled, setProxyEnabled] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [scanResult, setScanResult] = useState<string[] | null>(null);

  useEffect(() => {
    getSystemProxyOnce().then(setProxyEnabled);
  }, []);

  const toggleRunning = async () => {
    setBusy(status.running ? "stop" : "start");
    setActionError(null);
    try {
      if (status.running) await stop();
      else await start();
    } catch (e) {
      setActionError(String(e));
    } finally {
      setBusy(null);
    }
  };

  const toggleProxy = async (v: boolean) => {
    setProxyEnabled(v);
    try {
      await setSystemProxy(v);
    } catch (e) {
      setActionError(String(e));
      setProxyEnabled(!v);
    }
  };

  const onRegister = async () => {
    setBusy("register");
    setActionError(null);
    try {
      const res = await register();
      setNotice(res.existing ? "已存在注册，无需重复操作" : `注册成功（id=${res.id}）`);
    } catch (e) {
      setActionError(String(e));
    } finally {
      setBusy(null);
    }
  };

  const onScanEdges = async () => {
    setBusy("scan");
    setActionError(null);
    setScanResult(null);
    try {
      const edges = await scanEdges();
      setScanResult(edges);
      setNotice(`扫描完成，推荐 ${edges.length ? edges[0] : "无"} 等 ${edges.length} 个端点`);
    } catch (e) {
      setActionError(String(e));
    } finally {
      setBusy(null);
    }
  };

  const onDeregister = async () => {
    if (!window.confirm("确定注销？注销后需重新注册才能使用代理。")) return;
    setBusy("deregister");
    setActionError(null);
    try {
      await deregister();
      setNotice("已注销");
    } catch (e) {
      setActionError(String(e));
    } finally {
      setBusy(null);
    }
  };

  return (
    <div className="space-y-4">
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
            <p className="mt-1 font-mono text-sm text-slate-900 dark:text-slate-100">
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
          <Button onClick={onScanEdges} loading={busy === "scan"} variant="secondary">
            <Radar className="h-4 w-4" /> 扫描最优边缘
          </Button>
          {scanResult && scanResult.length > 0 && (
            <span className="font-mono text-xs text-slate-600 dark:text-slate-300">
              推荐端点：{scanResult.join(" · ")}
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
          action={
            <Button
              onClick={onDeregister}
              loading={busy === "deregister"}
              variant="danger"
              className="h-8 px-3 text-xs"
            >
              <Trash2 className="h-3.5 w-3.5" /> 注销
            </Button>
          }
        >
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
            <div>
              <p className="text-xs text-slate-500 dark:text-slate-400">设备 ID</p>
              <p className="mt-1 break-all font-mono text-xs text-slate-900 dark:text-slate-100">
                {status.registration.id || "—"}
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
              <p className="text-xs text-slate-500 dark:text-slate-400">隧道类型</p>
              <p className="mt-1 font-mono text-sm text-slate-900 dark:text-slate-100">
                {status.registration.tunnelType || "masque"}
              </p>
            </div>
          </div>
          {notice && (
            <p className="mt-3 text-sm text-emerald-600 dark:text-emerald-400">{notice}</p>
          )}
        </Card>
      )}

      <Card title="流量统计">
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
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
        </div>
      </Card>

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

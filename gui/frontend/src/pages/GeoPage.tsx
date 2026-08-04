import { useCallback, useEffect, useState } from "react";
import { RefreshCw, Database, Clock } from "lucide-react";
import { getGeo, updateGeo, isDemoMode } from "../lib/api";
import { GeoInfo } from "../lib/types";
import { Button, Card, StatusPill } from "../components/ui";

export default function GeoPage() {
  const [geo, setGeo] = useState<GeoInfo | null>(null);
  const [demo, setDemo] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    try {
      // getGeo 已返回 fromGeo 归一化后的 GeoInfo，不要再包一层 fromGeo
      // （双重归一化，v0.5.7 与 StatusPage 同源 bug）。
      setGeo(await getGeo());
      setError(null);
    } catch (e) {
      setError(String(e));
    }
  }, []);

  useEffect(() => {
    void isDemoMode().then(setDemo);
    void refresh();
  }, [refresh]);

  const onUpdate = async () => {
    setBusy(true);
    setError(null);
    setNotice(null);
    try {
      const res = await updateGeo();
      setNotice(res.message);
      if (res.ok) void refresh();
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  };

  const fmt = (s?: string) => s ?? "—";
  const geoReady = geo?.geositeUpdated !== undefined && geo?.geoipUpdated !== undefined;

  return (
    <div className="space-y-4">
      <Card
        title="GEO 数据库"
        action={
          geo ? (
            <StatusPill ok={geoReady} text={geoReady ? "已就绪" : "未下载"} />
          ) : (
            <StatusPill ok={false} text="加载中" />
          )
        }
      >
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
          <div className="rounded-lg border border-slate-200 p-4 dark:border-slate-800">
            <p className="flex items-center gap-2 text-xs text-slate-500 dark:text-slate-400">
              <Database className="h-3.5 w-3.5" /> geosite.dat
            </p>
            <p className="mt-1 break-all font-mono text-sm text-slate-900 dark:text-slate-100">
              {geo ? geo.geositePath : "…"}
            </p>
            <p className="mt-1 text-xs text-slate-500 dark:text-slate-400">
              {geo?.geositeUpdated ? `更新于 ${fmt(geo.geositeUpdated)}` : "未下载"}
            </p>
          </div>
          <div className="rounded-lg border border-slate-200 p-4 dark:border-slate-800">
            <p className="flex items-center gap-2 text-xs text-slate-500 dark:text-slate-400">
              <Database className="h-3.5 w-3.5" /> geoip-lite.dat
            </p>
            <p className="mt-1 break-all font-mono text-sm text-slate-900 dark:text-slate-100">
              {geo ? geo.geoipPath : "…"}
            </p>
            <p className="mt-1 text-xs text-slate-500 dark:text-slate-400">
              {geo?.geoipUpdated ? `更新于 ${fmt(geo.geoipUpdated)}` : "未下载"}
            </p>
          </div>
        </div>

        <div className="mt-4 grid grid-cols-1 gap-4 sm:grid-cols-2">
          <div>
            <p className="text-xs text-slate-500 dark:text-slate-400">上游仓库</p>
            <p className="mt-1 font-mono text-sm text-slate-900 dark:text-slate-100">
              {geo?.repository ?? "…"}
            </p>
          </div>
          <div>
            <p className="flex items-center gap-1.5 text-xs text-slate-500 dark:text-slate-400">
              <Clock className="h-3.5 w-3.5" /> 自动更新
            </p>
            <p className="mt-1 text-sm text-slate-900 dark:text-slate-100">
              每 {geo?.autoUpdateDays ?? 7} 天
              {geo?.lastChecked ? `（上次检查 ${geo.lastChecked}）` : ""}
            </p>
          </div>
        </div>

        <div className="mt-4">
          <p className="text-xs text-slate-500 dark:text-slate-400">下载地址</p>
          <p className="mt-1 break-all font-mono text-xs text-slate-700 dark:text-slate-300">
            {geo?.baseURL ?? "…"}
          </p>
        </div>

        <div className="mt-5 flex flex-wrap items-center gap-3">
          <Button onClick={onUpdate} loading={busy}>
            <RefreshCw className="h-4 w-4" /> 立即更新
          </Button>
          {notice && (
            <span className="text-sm text-emerald-600 dark:text-emerald-400">{notice}</span>
          )}
          {error && <span className="text-sm text-red-600 dark:text-red-400">{error}</span>}
          {demo && (
            <span className="text-xs text-slate-400">演示模式：数据为模拟值</span>
          )}
        </div>
      </Card>
    </div>
  );
}

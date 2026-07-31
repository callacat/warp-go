import { useEffect, useState } from "react";
import { Save, RotateCcw } from "lucide-react";
import { getConfig, saveConfig, isDemoMode } from "../lib/api";
import { fromConfig, AppConfig } from "../lib/types";
import { Button, Card, Field, inputCls } from "../components/ui";

export default function SettingsPage() {
  const [cfg, setCfg] = useState<AppConfig | null>(null);
  const [demo, setDemo] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  useEffect(() => {
    void isDemoMode().then(setDemo);
    void load();
  }, []);

  const load = async () => {
    try {
      setCfg(fromConfig(await getConfig()));
      setError(null);
    } catch (e) {
      setError(String(e));
    }
  };

  const set = <K extends keyof AppConfig>(key: K, value: AppConfig[K]) => {
    setCfg((c) => (c ? { ...c, [key]: value } : c));
    setNotice(null);
  };

  const onSave = async () => {
    if (!cfg) return;
    setBusy(true);
    setError(null);
    setNotice(null);
    try {
      await saveConfig(cfg);
      setNotice("配置已保存（文件变更将触发热重载）");
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-4">
      <Card title="基本设置" action={demo ? <span className="text-xs text-slate-400">演示模式</span> : undefined}>
        {cfg ? (
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <Field label="监听地址" hint="代理监听 host:port">
              <input
                className={inputCls}
                value={cfg.listen}
                onChange={(e) => set("listen", e.target.value)}
              />
            </Field>
            <Field label="规则文件" hint="rules.txt 路径（相对执行目录）">
              <input
                className={inputCls}
                value={cfg.rulesPath}
                onChange={(e) => set("rulesPath", e.target.value)}
              />
            </Field>
            <Field label="GEO 目录" hint="geosite.dat / geoip-lite.dat 存放目录">
              <input
                className={inputCls}
                value={cfg.geoDir}
                onChange={(e) => set("geoDir", e.target.value)}
              />
            </Field>
            <Field label="自动更新间隔（天）" hint="GEO 数据库自动检查更新">
              <input
                type="number"
                min={0}
                className={inputCls}
                value={cfg.autoUpdateDays}
                onChange={(e) => set("autoUpdateDays", Math.max(0, Number(e.target.value) || 0))}
              />
            </Field>
            <Field label="GEO 仓库" hint="格式 owner/repo">
              <input
                className={inputCls}
                value={cfg.geoRepo}
                onChange={(e) => set("geoRepo", e.target.value)}
              />
            </Field>
            <Field label="GEO 下载 URL" hint="Release 下载基础地址">
              <input
                className={inputCls}
                value={cfg.geoBaseURL}
                onChange={(e) => set("geoBaseURL", e.target.value)}
              />
            </Field>
            <Field label="日志目录" hint="logs 目录（相对执行目录，可选）">
              <input
                className={inputCls}
                value={cfg.logDir}
                onChange={(e) => set("logDir", e.target.value)}
              />
            </Field>
          </div>
        ) : (
          <p className="text-sm text-slate-500 dark:text-slate-400">加载中…</p>
        )}

        <div className="mt-5 flex flex-wrap items-center gap-3">
          <Button onClick={onSave} loading={busy} disabled={!cfg}>
            <Save className="h-4 w-4" /> 保存配置
          </Button>
          <Button onClick={load} variant="secondary" disabled={!cfg}>
            <RotateCcw className="h-4 w-4" /> 重新加载
          </Button>
          {notice && (
            <span className="text-sm text-emerald-600 dark:text-emerald-400">{notice}</span>
          )}
          {error && <span className="text-sm text-red-600 dark:text-red-400">{error}</span>}
        </div>
        <p className="mt-3 text-xs text-slate-500 dark:text-slate-400">
          配置写入执行目录下的 config.json；文件被外部修改时会自动热重载。
        </p>
      </Card>
    </div>
  );
}

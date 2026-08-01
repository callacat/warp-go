import { useEffect, useState } from "react";
import { Save, RotateCcw, Rocket } from "lucide-react";
import {
  getAutostartEnabled,
  getConfig,
  isDemoMode,
  saveConfig,
  setAutostart,
} from "../lib/api";
import { fromConfig, AppConfig } from "../lib/types";
import { Button, Card, Field, Toggle, inputCls } from "../components/ui";

export default function SettingsPage() {
  const [cfg, setCfg] = useState<AppConfig | null>(null);
  const [demo, setDemo] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [autostart, setAutostartState] = useState(false);
  const [autostartBusy, setAutostartBusy] = useState(false);

  useEffect(() => {
    void isDemoMode().then(setDemo);
    void load();
    getAutostartEnabled().then(setAutostartState).catch(() => {});
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

  const toggleAutostart = async (v: boolean) => {
    setAutostartBusy(true);
    setError(null);
    try {
      await setAutostart(v);
      setAutostartState(v);
      setNotice(v ? "已开启开机自启" : "已关闭开机自启");
    } catch (e) {
      setError(String(e));
    } finally {
      setAutostartBusy(false);
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
            <Field
              label="下载加速前缀"
              hint="GitHub 加速（如 https://gh-proxy.org/），置空关闭"
            >
              <input
                className={inputCls}
                value={cfg.downloadProxy}
                onChange={(e) => set("downloadProxy", e.target.value)}
                placeholder="https://gh-proxy.org/"
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
            <RotateCcw className="h-4 w-4" /> 重置配置
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

      <Card title="开机自启">
        <div className="flex items-center justify-between gap-4">
          <div className="flex items-start gap-3">
            <Rocket className="mt-0.5 h-5 w-5 text-orange-500" />
            <div>
              <p className="text-sm font-medium">登录后自动启动</p>
              <p className="text-xs text-slate-500 dark:text-slate-400">
                系统登录时自动运行 warp-gui（Windows 注册表 / macOS LaunchAgent / Linux autostart）
              </p>
            </div>
          </div>
          <Toggle
            checked={autostart}
            onChange={toggleAutostart}
            disabled={autostartBusy}
          />
        </div>
      </Card>
    </div>
  );
}

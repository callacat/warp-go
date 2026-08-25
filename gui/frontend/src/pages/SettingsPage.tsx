import { useEffect, useState, useCallback } from "react";
import { Monitor, Moon, Palette, Rocket, RotateCcw, Save, Sun } from "lucide-react";
import {
  checkUpdate,
  getAutostartEnabled,
  getConfig,
  getVersion,
  isDemoMode,
  openExternalBrowser,
  saveConfig,
  setAutostart,
} from "../lib/api";
import { AppConfig } from "../lib/types";
import { useThemeContext } from "../lib/ThemeContext";
import type { ThemeMode } from "../lib/theme";
import { Button, Card, Field, Toggle, inputCls } from "../components/ui";

const THEME_OPTIONS: { value: ThemeMode; label: string; icon: typeof Sun }[] = [
  { value: "light", label: "浅色", icon: Sun },
  { value: "dark", label: "深色", icon: Moon },
  { value: "system", label: "跟随系统", icon: Monitor },
];

export default function SettingsPage() {
  const [cfg, setCfg] = useState<AppConfig | null>(null);
  const [demo, setDemo] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [autostart, setAutostartState] = useState(false);
  const [autostartBusy, setAutostartBusy] = useState(false);
  const [version, setVersion] = useState<string>("…");
  const [updateInfo, setUpdateInfo] = useState<string | null>(null);
  const [updateUrl, setUpdateUrl] = useState<string | null>(null);
  const [checking, setChecking] = useState(false);
  const { mode, setMode, setModeFromConfig } = useThemeContext();

  const load = useCallback(async () => {
    try {
      // getConfig 已返回 fromConfig 归一化后的 AppConfig，不要再包一层
      // fromConfig（双重归一化，v0.5.7 与 StatusPage 同源 bug）。
      const config = await getConfig();
      setCfg(config);
      setModeFromConfig(config);
      setError(null);
    } catch (e) {
      setError(String(e));
    }
  }, [setModeFromConfig]);

  useEffect(() => {
    void isDemoMode().then(setDemo);
    void load();
    getAutostartEnabled().then(setAutostartState).catch(() => {});
    getVersion().then(setVersion).catch(() => {});
  }, [load]);

  const onCheckUpdate = async () => {
    setChecking(true);
    setUpdateInfo(null);
    setUpdateUrl(null);
    try {
      const info = await checkUpdate();
      if (info.has_update) {
        setUpdateInfo(`发现新版本 ${info.tag}（当前 ${version}）`);
        setUpdateUrl(info.url || null);
      } else if (info.latest && info.latest !== "dev") {
        setUpdateInfo(`已是最新版本 ${info.latest}`);
      } else {
        setUpdateInfo("当前为开发版，无法比较版本");
      }
    } catch (e) {
      setUpdateInfo(`检查失败：${String(e)}`);
    } finally {
      setChecking(false);
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
      setNotice("配置已保存（重启后生效）");
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
      <Card title="关于">
        <div className="flex items-center gap-3">
          <Rocket className="h-5 w-5 text-orange-500" />
          <div className="min-w-0 flex-1">
            <p className="text-sm font-medium">warp-go {version}</p>
            <p className="text-xs text-slate-500 dark:text-slate-400">
              Cloudflare WARP 客户端（MASQUE over QUIC/HTTP-3）
            </p>
          </div>
          <Button variant="secondary" onClick={onCheckUpdate} disabled={checking}>
            {checking ? "检查中…" : "检查更新"}
          </Button>
        </div>
        {updateInfo && (
          <p className="mt-3 text-sm">
            <span className={updateUrl ? "text-amber-600 dark:text-amber-400" : "text-emerald-600 dark:text-emerald-400"}>
              {updateInfo}
            </span>
            {updateUrl && (
              <button
                type="button"
                onClick={() => openExternalBrowser(updateUrl)}
                className="ml-2 text-orange-600 underline dark:text-orange-400"
              >
                前往下载
              </button>
            )}
          </p>
        )}
      </Card>

      <Card title="外观">
        <div className="flex items-start gap-3">
          <Palette className="mt-0.5 h-5 w-5 text-orange-500" />
          <div>
            <p className="text-sm font-medium">主题模式</p>
            <p className="text-xs text-slate-500 dark:text-slate-400">
              跟随系统时自动同步操作系统的明暗主题
            </p>
          </div>
        </div>
        <div className="mt-4 grid grid-cols-3 gap-1 rounded-lg border border-slate-200 bg-slate-50 p-1 dark:border-slate-700 dark:bg-slate-800">
          {THEME_OPTIONS.map(({ value, label, icon: Icon }) => (
            <button
              key={value}
              type="button"
              onClick={() => setMode(value)}
              aria-pressed={mode === value}
              className={`flex items-center justify-center gap-1.5 rounded-md px-2 py-2 text-sm font-medium transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-orange-500/50 ${
                mode === value
                  ? "bg-white text-orange-600 shadow-sm dark:bg-slate-700 dark:text-orange-400"
                  : "text-slate-600 hover:text-slate-900 dark:text-slate-400 dark:hover:text-slate-200"
              }`}
            >
              <Icon className="h-4 w-4 shrink-0" />
              {label}
            </button>
          ))}
        </div>
      </Card>

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
          配置写入执行目录下的 config.json；修改需重启生效（取消热加载）。
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

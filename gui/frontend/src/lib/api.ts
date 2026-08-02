// API bridge between the React UI and the Wails Go backend.
//
// The real backend methods live in `gui/service.go` (parallel task) and are
// surfaced to the frontend as generated TypeScript bindings under
// `frontend/bindings/gui/` (produced by `wails3 generate bindings`).
//
// Until those bindings exist, a compile-time placeholder occupies the same
// path and carries the `__MOCK_BINDINGS__` marker. This module detects the
// marker at runtime:
//   - placeholder present (standalone `npm run build`/`npm run dev`) → all
//     calls resolve to realistic demo data, so the UI is fully exercisable;
//   - real bindings present (Wails app) → calls go straight to Go.
//
// The structural `ServiceAPI` interface keeps this file decoupled from the
// exact generated typings, so regenerating bindings never breaks it.

import {
  AppConfig,
  AppStatus,
  fromConfig,
  fromGeo,
  fromLogs,
  fromStatus,
  GeoInfo,
  LogEntry,
} from "./types";

// ---------- backend service shape (structural, mirror of gui/service.go) ----------

interface ServiceAPI {
  GetStatus(): Promise<unknown>;
  Start(): Promise<unknown>;
  Stop(): Promise<unknown>;
  Register(): Promise<unknown>;
  Deregister(): Promise<unknown>;
  GetRules(): Promise<unknown>;
  SaveRules(rulesText: string): Promise<unknown>;
  ReloadRules(): Promise<unknown>;
  GetGeo(): Promise<unknown>;
  UpdateGeo(): Promise<unknown>;
  SetSystemProxy(enabled: boolean): Promise<unknown>;
  GetSystemProxyEnabled(): Promise<unknown>;
  ScanEdges(): Promise<unknown>;
  ScanEdgesV4(): Promise<unknown>;
  ScanEdgesV6(): Promise<unknown>;
  ApplyEdge(addr: string): Promise<unknown>;
  SetAutostart(enabled: boolean): Promise<unknown>;
  GetAutostartEnabled(): Promise<unknown>;
  GetConfig(): Promise<unknown>;
  SaveConfig(config: Record<string, unknown>): Promise<unknown>;
  GetLogs(limit: number): Promise<unknown>;
  GetVersion(): Promise<unknown>;
  CheckUpdate(): Promise<unknown>;
}

// ---------- demo data (used while bindings are placeholders) ----------

const DEFAULT_RULES = `# warp-go 路由规则（每行：行为,条件；# 为注释）
REJECT,geosite:category-ads-all
direct,geosite:private
direct,geoip:private
proxy,geosite:google
proxy,geosite:geolocation-!cn
direct,geosite:cn
direct,geoip:cn
`;

const mockState = {
  running: false,
  sysProxy: false,
  autostart: false,
  rulesText: DEFAULT_RULES,
  counters: { proxy: 128, direct: 947, miss: 14, rejected: 23 },
  startedAt: undefined as string | undefined,
  logs: [
    { time: "19:40:02", level: "info", msg: "warp-go GUI（演示模式）" },
    { time: "19:40:02", level: "info", msg: "已加载配置 config.json" },
    { time: "19:40:03", level: "info", msg: "规则引擎就绪：6 条规则" },
    { time: "19:40:03", level: "debug", msg: "GEO 数据库命中缓存" },
    { time: "19:41:27", level: "warn", msg: "重连：QUIC 空闲超时，按代际恢复" },
    { time: "19:41:28", level: "info", msg: "已建立 MASQUE 隧道（H3）" },
  ] as LogEntry[],
};

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
const jitter = (base: number) => base + Math.random() * 220;

// ---------- service resolution ----------

let svcPromise: Promise<ServiceAPI | null> | null = null;

function loadService(): Promise<ServiceAPI | null> {
  if (!svcPromise) {
    svcPromise = (async () => {
      try {
        // Wails v3 把 bindings 生成到 frontend/bindings/warp/gui/（带 module 路径）；
        // 之前误用 bindings/gui/ 导致永远命中占位/演示模式。
        // @ts-expect-error - bindings 是 .js 无 .d.ts（TS7016），运行时由 Wails 桥接
        const mod = (await import("../../bindings/warp/gui/index.js")) as {
          Service?: Record<string, unknown>;
        };
        const ns = mod.Service;
        if (!ns) return null;
        // Placeholder stand-in? -> use demo data instead of calling $Call.
        if (ns.__MOCK_BINDINGS__ === true) return null;
        return ns as unknown as ServiceAPI;
      } catch {
        return null;
      }
    })();
  }
  return svcPromise;
}

/** True when running standalone without the Wails bridge. */
export async function isDemoMode(): Promise<boolean> {
  return (await loadService()) === null;
}

// ---------- mock implementation ----------

function mockStatus(): AppStatus {
  return {
    running: mockState.running,
    listening: "127.0.0.1:40000",
    startedAt: mockState.startedAt,
    registered: true,
    registration: {
      id: "demo-reg-id",
      assignedIPv4: "172.16.0.2",
      assignedIPv6: "2606:4700:100::2",
      endpointV4: "162.159.192.5",
      tunnelType: "masque",
    },
    counters: { ...mockState.counters },
  };
}

function mockConfig(): AppConfig {
  return {
    listen: "127.0.0.1:40000",
    rulesPath: "rules.txt",
    geoDir: "geo",
    geoRepo: "MetaCubeX/meta-rules-dat",
    geoBaseURL: "https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest",
    autoUpdateDays: 7,
    logDir: "logs",
    systemProxy: mockState.sysProxy,
    allowUDP: false,
    downloadProxy: "https://gh-proxy.org/",
  };
}

function mockGeo(): GeoInfo {
  return {
    geositePath: "geo/geosite.dat",
    geoipPath: "geo/geoip-lite.dat",
    geositeUpdated: "2026-07-30 04:00:00",
    geoipUpdated: "2026-07-30 04:00:00",
    repository: "MetaCubeX/meta-rules-dat",
    baseURL: "https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest",
    autoUpdateDays: 7,
    lastChecked: "2026-07-31 04:00:00",
  };
}

// ---------- public API ----------

export async function getStatus(): Promise<AppStatus> {
  const svc = await loadService();
  if (!svc) {
    await sleep(jitter(150));
    return mockStatus();
  }
  return fromStatus(await svc.GetStatus());
}

export async function start(): Promise<void> {
  const svc = await loadService();
  if (!svc) {
    await sleep(jitter(300));
    mockState.running = true;
    mockState.startedAt = new Date().toLocaleString("zh-CN");
    mockState.logs.push({ time: now(), level: "info", msg: "代理已启动（演示）" });
    return;
  }
  await svc.Start();
}

export async function stop(): Promise<void> {
  const svc = await loadService();
  if (!svc) {
    await sleep(jitter(300));
    mockState.running = false;
    mockState.startedAt = undefined;
    mockState.logs.push({ time: now(), level: "info", msg: "代理已停止（演示）" });
    return;
  }
  await svc.Stop();
}

export interface RegisterResult {
  existing: boolean;
  id: string;
}

export async function register(): Promise<RegisterResult> {
  const svc = await loadService();
  if (!svc) {
    await sleep(jitter(400));
    mockState.logs.push({ time: now(), level: "info", msg: "已注册（演示）" });
    return { existing: false, id: "demo-id" };
  }
  const raw = (await svc.Register()) as { existing?: boolean; id?: string } | null;
  return { existing: raw?.existing ?? false, id: raw?.id ?? "" };
}

export async function deregister(): Promise<void> {
  const svc = await loadService();
  if (!svc) {
    await sleep(jitter(300));
    mockState.logs.push({ time: now(), level: "info", msg: "已注销（演示）" });
    return;
  }
  await svc.Deregister();
}

export async function getRules(): Promise<string> {
  const svc = await loadService();
  if (!svc) {
    await sleep(jitter(150));
    return mockState.rulesText;
  }
  const raw = await svc.GetRules();
  return typeof raw === "string" ? raw : String(raw ?? "");
}

export async function saveRules(rulesText: string): Promise<void> {
  const svc = await loadService();
  if (!svc) {
    await sleep(jitter(250));
    mockState.rulesText = rulesText;
    mockState.logs.push({ time: now(), level: "info", msg: "规则已保存（演示）" });
    return;
  }
  await svc.SaveRules(rulesText);
}

export async function reloadRules(): Promise<void> {
  const svc = await loadService();
  if (!svc) {
    await sleep(jitter(200));
    mockState.logs.push({ time: now(), level: "info", msg: "规则已热重载（演示）" });
    return;
  }
  await svc.ReloadRules();
}

export async function getGeo(): Promise<GeoInfo> {
  const svc = await loadService();
  if (!svc) {
    await sleep(jitter(150));
    return mockGeo();
  }
  return fromGeo(await svc.GetGeo());
}

export interface UpdateGeoResult {
  ok: boolean;
  message: string;
}

export async function updateGeo(): Promise<UpdateGeoResult> {
  const svc = await loadService();
  if (!svc) {
    await sleep(jitter(600));
    mockState.logs.push({
      time: now(),
      level: "info",
      msg: "GEO 数据已更新（演示）：geosite.dat / geoip-lite.dat",
    });
    return { ok: true, message: "GEO 数据已是最新（演示）" };
  }
  const raw = await svc.UpdateGeo();
  const r = raw as { ok?: boolean; message?: string } | null;
  // Wails bindings 返回 UpdateGeoResult 对象（{ok, message}），不是 Error。
  return {
    ok: r?.ok === true,
    message: r?.message ?? (r?.ok ? "GEO 数据已更新" : "GEO 数据更新失败"),
  };
}

export async function setSystemProxy(enabled: boolean): Promise<void> {
  const svc = await loadService();
  if (!svc) {
    await sleep(jitter(250));
    mockState.sysProxy = enabled;
    mockState.logs.push({
      time: now(),
      level: "info",
      msg: `系统代理已${enabled ? "启用" : "关闭"}（演示）`,
    });
    return;
  }
  await svc.SetSystemProxy(enabled);
}

export async function getSystemProxyEnabled(): Promise<boolean> {
  const svc = await loadService();
  if (!svc) {
    await sleep(jitter(120));
    return mockState.sysProxy;
  }
  return (await svc.GetSystemProxyEnabled()) === true;
}

export async function scanEdges(): Promise<string[]> {
  return scanEdgesFamily(null);
}

export async function scanEdgesV4(): Promise<string[]> {
  return scanEdgesFamily("v4");
}

export async function scanEdgesV6(): Promise<string[]> {
  return scanEdgesFamily("v6");
}

async function scanEdgesFamily(variant: "v4" | "v6" | null): Promise<string[]> {
  const svc = await loadService();
  if (!svc) {
    await sleep(jitter(900));
    const demo = variant === "v6"
      ? ["2606:4700:103::2:443", "2606:4700:104::2:443"]
      : ["162.159.192.5:4500", "162.159.193.10:4500", "162.159.195.3:4500"];
    return demo;
  }
  const raw = variant === "v6" ? await svc.ScanEdgesV6() : variant === "v4" ? await svc.ScanEdgesV4() : await svc.ScanEdges();
  return Array.isArray(raw) ? (raw as string[]) : [];
}

export async function applyEdge(addr: string): Promise<void> {
  const svc = await loadService();
  if (!svc) {
    await sleep(jitter(200));
    mockState.logs.push({ time: now(), level: "info", msg: `已应用边缘 ${addr}（演示）` });
    return;
  }
  await svc.ApplyEdge(addr);
}

export async function setAutostart(enabled: boolean): Promise<void> {
  const svc = await loadService();
  if (!svc) {
    await sleep(jitter(200));
    mockState.autostart = enabled;
    return;
  }
  await svc.SetAutostart(enabled);
}

export async function getAutostartEnabled(): Promise<boolean> {
  const svc = await loadService();
  if (!svc) {
    await sleep(jitter(120));
    return mockState.autostart;
  }
  return (await svc.GetAutostartEnabled()) === true;
}

export async function getConfig(): Promise<AppConfig> {
  const svc = await loadService();
  if (!svc) {
    await sleep(jitter(150));
    return mockConfig();
  }
  return fromConfig(await svc.GetConfig());
}

export async function saveConfig(config: AppConfig): Promise<void> {
  const svc = await loadService();
  if (!svc) {
    await sleep(jitter(250));
    mockState.logs.push({ time: now(), level: "info", msg: "配置已保存（演示）" });
    return;
  }
  // 前端 AppConfig 是 camelCase，但 Go core.Config 的 JSON tag 是 snake_case；
  // 直接传对象（不要 stringify），Wails 会按字段名映射。
  await svc.SaveConfig({
    listen_addr: config.listen,
    rules_path: config.rulesPath,
    geo_dir: config.geoDir,
    geo_repo: config.geoRepo,
    geo_auto_update_days: config.autoUpdateDays,
    enable_system_proxy: config.systemProxy,
    allow_udp: config.allowUDP,
    download_proxy: config.downloadProxy,
  });
}

export async function getLogs(limit = 200): Promise<LogEntry[]> {
  const svc = await loadService();
  if (!svc) {
    await sleep(jitter(120));
    return [...mockState.logs].slice(-limit);
  }
  return fromLogs(await svc.GetLogs(limit));
}

export async function getVersion(): Promise<string> {
  const svc = await loadService();
  if (!svc) {
    await sleep(jitter(60));
    return "dev";
  }
  const raw = await svc.GetVersion();
  return typeof raw === "string" ? raw : "dev";
}

export interface UpdateInfo {
  current: string;
  latest: string;
  has_update: boolean;
  url: string;
  tag: string;
}

export async function checkUpdate(): Promise<UpdateInfo> {
  const svc = await loadService();
  if (!svc) {
    await sleep(jitter(400));
    return { current: "dev", latest: "dev", has_update: false, url: "", tag: "" };
  }
  const raw = await svc.CheckUpdate();
  return (raw as UpdateInfo) ?? { current: "", latest: "", has_update: false, url: "", tag: "" };
}

function now(): string {
  return new Date().toLocaleTimeString("zh-CN", { hour12: false });
}

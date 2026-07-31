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
  GetConfig(): Promise<unknown>;
  SaveConfig(configJson: string): Promise<unknown>;
  GetLogs(limit: number): Promise<unknown>;
}

// ---------- demo data (used while bindings are placeholders) ----------

const DEFAULT_RULES = `# warp-go 路由规则（每行：行为,条件；# 为注释）
proxy,geosite:google
proxy,geosite:geolocation-!cn
direct,geoip:private
direct,geosite:private
direct,geosite:cn
direct,geoip:cn
`;

const mockState = {
  running: false,
  sysProxy: false,
  rulesText: DEFAULT_RULES,
  counters: { proxy: 128, direct: 947, miss: 14 },
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
  return {
    ok: !(raw instanceof Error),
    message: raw instanceof Error ? raw.message : "GEO 数据已更新",
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
  await svc.SaveConfig(JSON.stringify(config));
}

export async function getLogs(limit = 200): Promise<LogEntry[]> {
  const svc = await loadService();
  if (!svc) {
    await sleep(jitter(120));
    return [...mockState.logs].slice(-limit);
  }
  return fromLogs(await svc.GetLogs(limit));
}

function now(): string {
  return new Date().toLocaleTimeString("zh-CN", { hour12: false });
}

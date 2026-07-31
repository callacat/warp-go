// Shared types for the warp-go GUI frontend.
//
// The Go backend contract (gui/service.go) is owned by the parallel task and
// may still evolve. Every type here comes with a defensive normalizer
// (`from*`) so the UI never crashes on missing/renamed fields — unknown
// shapes degrade to safe defaults.

export interface ProxyCounters {
  proxy: number;
  direct: number;
  miss: number;
}

export interface RegistrationInfo {
  id: string;
  account?: string;
  keyType?: string;
  tunnelType?: string;
  endpointV4?: string;
  endpointV6?: string;
  endpointPorts?: number[];
  assignedIPv4?: string;
  assignedIPv6?: string;
}

export interface AppStatus {
  running: boolean;
  listening: string;
  startedAt?: string;
  error?: string;
  registered: boolean;
  counters: ProxyCounters;
  registration: RegistrationInfo | null;
}

export interface AppConfig {
  listen: string;
  rulesPath: string;
  geoDir: string;
  geoRepo: string;
  geoBaseURL: string;
  autoUpdateDays: number;
  logDir: string;
  systemProxy: boolean;
}

export type RuleAction = "proxy" | "direct";

export interface RuleEntry {
  line: number;
  action: RuleAction;
  condition: string;
}

export interface GeoInfo {
  geositePath: string;
  geoipPath: string;
  geositeUpdated?: string;
  geoipUpdated?: string;
  repository: string;
  baseURL: string;
  autoUpdateDays: number;
  lastChecked?: string;
}

export type LogLevel = "debug" | "info" | "warn" | "error";

export interface LogEntry {
  time: string;
  level: LogLevel;
  msg: string;
}

// ---------- defensive normalizers ----------

const num = (v: unknown, d: number): number =>
  typeof v === "number" && Number.isFinite(v) ? v : d;
const str = (v: unknown, d: string): string =>
  typeof v === "string" && v.length > 0 ? v : d;

export function fromCounters(v: unknown): ProxyCounters {
  const o = (v ?? {}) as Record<string, unknown>;
  return {
    proxy: num(o.proxy, 0),
    direct: num(o.direct, 0),
    miss: num(o.miss, 0),
  };
}

export function fromStatus(v: unknown): AppStatus {
  const o = (v ?? {}) as Record<string, unknown>;
  return {
    running: o.running === true,
    listening: str(o.listening, "127.0.0.1:40000"),
    startedAt: typeof o.startedAt === "string" ? o.startedAt : undefined,
    error: typeof o.error === "string" ? o.error : undefined,
    registered: o.registered === true || o.Registered === true,
    counters: fromCounters(o.counters),
    registration: fromRegistration(o.registration),
  };
}

function fromRegistration(v: unknown): RegistrationInfo | null {
  const o = (v ?? null) as Record<string, unknown> | null;
  if (!o) return null;
  return {
    id: str(o.id, ""),
    account: typeof o.account === "string" ? o.account : undefined,
    keyType: typeof o.key_type === "string" ? o.key_type : undefined,
    tunnelType: typeof o.tunnel_type === "string" ? o.tunnel_type : undefined,
    endpointV4: typeof o.endpoint_v4 === "string" ? o.endpoint_v4 : undefined,
    endpointV6: typeof o.endpoint_v6 === "string" ? o.endpoint_v6 : undefined,
    endpointPorts: Array.isArray(o.endpoint_ports) ? (o.endpoint_ports as number[]) : undefined,
    assignedIPv4: typeof o.assigned_ipv4 === "string" ? o.assigned_ipv4 : undefined,
    assignedIPv6: typeof o.assigned_ipv6 === "string" ? o.assigned_ipv6 : undefined,
  };
}

export function fromConfig(v: unknown): AppConfig {
  const o = (v ?? {}) as Record<string, unknown>;
  return {
    listen: str(o.listen, "127.0.0.1:40000"),
    rulesPath: str(o.rulesPath, "rules.txt"),
    geoDir: str(o.geoDir, "geo"),
    geoRepo: str(o.geoRepo, "MetaCubeX/meta-rules-dat"),
    geoBaseURL: str(
      o.geoBaseURL,
      "https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest",
    ),
    autoUpdateDays: num(o.autoUpdateDays, 7),
    logDir: str(o.logDir, "logs"),
    systemProxy: o.systemProxy === true,
  };
}

export function fromGeo(v: unknown): GeoInfo {
  const o = (v ?? {}) as Record<string, unknown>;
  return {
    geositePath: str(o.geositePath, "geo/geosite.dat"),
    geoipPath: str(o.geoipPath, "geo/geoip-lite.dat"),
    geositeUpdated: typeof o.geositeUpdated === "string" ? o.geositeUpdated : undefined,
    geoipUpdated: typeof o.geoipUpdated === "string" ? o.geoipUpdated : undefined,
    repository: str(o.repository, "MetaCubeX/meta-rules-dat"),
    baseURL: str(
      o.baseURL,
      "https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest",
    ),
    autoUpdateDays: num(o.autoUpdateDays, 7),
    lastChecked: typeof o.lastChecked === "string" ? o.lastChecked : undefined,
  };
}

const LEVELS: LogLevel[] = ["debug", "info", "warn", "error"];

export function fromLogs(v: unknown): LogEntry[] {
  if (!Array.isArray(v)) return [];
  const out: LogEntry[] = [];
  for (const raw of v) {
    const o = (raw ?? {}) as Record<string, unknown>;
    const lv = str(o.level, "info").toLowerCase();
    out.push({
      time: str(o.time, ""),
      level: (LEVELS.includes(lv as LogLevel) ? lv : "info") as LogLevel,
      msg: str(o.msg, String(raw)),
    });
  }
  return out;
}

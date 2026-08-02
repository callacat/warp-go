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
  rejected: number;
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
  isAndroid: boolean;
  initDone: boolean;
  sysProxyOn: boolean;
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
  allowUDP: boolean;
  downloadProxy: string;
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
  // Go route.Stats 字段无 json tag → 序列化为大写键（ProxyHits 等）；
  // 兼容小写/演示模式的 camelCase 键。双向读取避免真实模式计数恒为 0。
  return {
    proxy: num(o.proxy, num(o.ProxyHits, 0)),
    direct: num(o.direct, num(o.DirectHits, 0)),
    miss: num(o.miss, num(o.Misses, 0)),
    rejected: num(o.rejected, num(o.RejectedHits, 0)),
  };
}

export function fromStatus(v: unknown): AppStatus {
  const o = (v ?? {}) as Record<string, unknown>;
  return {
    running: o.running === true || o.state === "running",
    listening: str(o.listen_addr ?? o.listening, "127.0.0.1:40000"),
    startedAt:
      typeof o.start_time === "string"
        ? o.start_time
        : typeof o.startedAt === "string"
          ? o.startedAt
          : undefined,
    error:
      typeof o.last_error === "string"
        ? o.last_error
        : typeof o.error === "string"
          ? o.error
          : undefined,
    registered: o.registered === true || o.Registered === true,
    isAndroid: o.is_android === true,
    initDone: o.init_done === true || o.InitDone === true,
    sysProxyOn: o.sys_proxy_on === true || o.SysProxyOn === true,
    counters: fromCounters(o.stats ?? o.counters),
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
  // Go core.Config JSON tag 是 snake_case；兼容 camelCase 兜底。
  return {
    listen: str(o.listen_addr ?? o.listen, "127.0.0.1:40000"),
    rulesPath: str(o.rules_path ?? o.rulesPath, "rules.txt"),
    geoDir: str(o.geo_dir ?? o.geoDir, "geo"),
    geoRepo: str(o.geo_repo ?? o.geoRepo, "MetaCubeX/meta-rules-dat"),
    geoBaseURL: str(
      o.geo_base_url ?? o.geoBaseURL,
      "https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest",
    ),
    autoUpdateDays: num(o.geo_auto_update_days ?? o.autoUpdateDays, 7),
    logDir: str(o.log_dir ?? o.logDir, "logs"),
    systemProxy: o.enable_system_proxy === true || o.systemProxy === true,
    allowUDP: o.allow_udp === true || o.allowUDP === true,
    downloadProxy: str(o.download_proxy ?? o.downloadProxy, "https://gh-proxy.org/"),
  };
}

export function fromGeo(v: unknown): GeoInfo {
  const o = (v ?? {}) as Record<string, unknown>;
  return {
    geositePath: str(o.geosite_path ?? o.geositePath, "geo/geosite.dat"),
    geoipPath: str(o.geoip_path ?? o.geoipPath, "geo/geoip-lite.dat"),
    geositeUpdated:
      typeof o.geosite_updated === "string"
        ? o.geosite_updated
        : typeof o.geositeUpdated === "string"
          ? o.geositeUpdated
          : undefined,
    geoipUpdated:
      typeof o.geoip_updated === "string"
        ? o.geoip_updated
        : typeof o.geoipUpdated === "string"
          ? o.geoipUpdated
          : undefined,
    repository: str(o.repository, "MetaCubeX/meta-rules-dat"),
    baseURL: str(
      o.base_url ?? o.baseURL,
      "https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest",
    ),
    autoUpdateDays: num(o.auto_update_days ?? o.autoUpdateDays, 7),
    lastChecked:
      typeof o.last_checked === "string"
        ? o.last_checked
        : typeof o.lastChecked === "string"
          ? o.lastChecked
          : undefined,
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

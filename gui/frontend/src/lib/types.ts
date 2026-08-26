// 前端契约类型: wails3 generate bindings 生成的 TS 为唯一类型源。
//
// from* 输入参数为 any（接受 ServiceAPI 的 unknown 返回值与测试部分对象），
// 输出为前端人体工学 camelCase 类型（命名自适应层）。
// 保留 fromLogs (level 校验降级) 与 fromStatus (state -> running 派生)。

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
  autoUpdateDays: number;
  systemProxy: boolean;
  allowUDP: boolean;
  downloadProxy: string;
  themeMode: "light" | "dark" | "system";
  perAppMode: "off" | "allow" | "disallow";
  perAppPackages: string[];
}

export interface PerAppConfig {
  mode: "off" | "allow" | "disallow";
  packages: string[];
}

export interface InstalledApp {
  package: string;
  label: string;
  system: boolean;
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

// ---------- bindings 适配 (生成类型 -> UI 类型) ----------

export function fromCounters(v: { proxy: number; direct: number; miss: number; rejected: number } | null | undefined): ProxyCounters {
  const o = (v ?? {}) as Record<string, number>;
  return {
    proxy: o.proxy ?? 0,
    direct: o.direct ?? 0,
    miss: o.miss ?? 0,
    rejected: o.rejected ?? 0,
  };
}

export function fromStatus(v: any): AppStatus {
  const o = v ?? {};
  return {
    running: o.state === "running",
    listening: o.listen_addr ?? "127.0.0.1:40000",
    startedAt: o.start_time,
    error: o.last_error,
    registered: o.registered === true,
    isAndroid: o.is_android === true,
    initDone: o.init_done === true,
    sysProxyOn: o.sys_proxy_on === true,
    counters: fromCounters(o.stats),
    registration: fromRegistration(o.registration),
  };
}

export function fromRegistration(
  v: any | null | undefined,
): RegistrationInfo | null {
  if (!v) return null;
  return {
    id: v.id,
    account: v.account || undefined,
    keyType: v.key_type || undefined,
    tunnelType: v.tunnel_type || undefined,
    endpointV4: v.endpoint_v4 || undefined,
    endpointV6: v.endpoint_v6 || undefined,
    endpointPorts: v.endpoint_ports,
    assignedIPv4: v.assigned_ipv4 || undefined,
    assignedIPv6: v.assigned_ipv6 || undefined,
  };
}

export function fromConfig(v: any): AppConfig {
  const o = v ?? {};
  const theme = o.theme_mode;
  return {
    listen: o.listen_addr ?? "127.0.0.1:40000",
    rulesPath: o.rules_path ?? "rules.txt",
    geoDir: o.geo_dir ?? "geo",
    geoRepo: o.geo_repo ?? "MetaCubeX/meta-rules-dat",
    autoUpdateDays: o.geo_auto_update_days ?? 7,
    systemProxy: o.enable_system_proxy === true,
    allowUDP: o.allow_udp === true,
    downloadProxy: o.download_proxy ?? "https://gh-proxy.org/",
    themeMode:
      theme === "light" || theme === "dark" || theme === "system"
        ? theme
        : "system",
    perAppMode: o.per_app_mode === "allow" || o.per_app_mode === "disallow" ? o.per_app_mode : "off",
    perAppPackages: Array.isArray(o.per_app_packages) ? o.per_app_packages : [],
  };
}

export function fromGeo(v: any): GeoInfo {
  const o = v ?? {};
  return {
    geositePath: o.geosite_path ?? "geo/geosite.dat",
    geoipPath: o.geoip_path ?? "geo/geoip-lite.dat",
    geositeUpdated: o.geosite_updated,
    geoipUpdated: o.geoip_updated,
    repository: o.repository ?? "MetaCubeX/meta-rules-dat",
    baseURL: o.base_url ?? "",
    autoUpdateDays: o.auto_update_days ?? 7,
    lastChecked: o.last_checked,
  };
}

const LEVELS: LogLevel[] = ["debug", "info", "warn", "error"];

export function fromLogs(v: any): LogEntry[] {
  if (!Array.isArray(v)) return [];
  const out: LogEntry[] = [];
  for (const raw of v) {
    const o = raw ?? {};
    const lv = (o.level ?? "info").toLowerCase();
    out.push({
      time: o.time ?? "",
      level: (LEVELS.includes(lv as LogLevel) ? lv : "info") as LogLevel,
      msg: o.msg ?? "",
    });
  }
  return out;
}

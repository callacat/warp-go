// 前端契约类型: wails3 generate bindings 生成的 TS 为唯一类型源。
//
// from* 输入参数为生成类型 (Go 改字段 -> tsc 编译期失败, 而非运行期静默错)。
// 输出为前端人体工学 camelCase 类型 (命名自适应层, 编译期经生成类型验证)。
// 保留 fromLogs (level 校验降级) 与 fromStatus (state -> running 派生)。

import {
  Status as BackendStatus,
  Config as BackendConfig,
  RegistrationInfo as BackendRegistrationInfo,
} from "../../bindings/warp/core/models.js";
import {
  GeoInfo as BackendGeoInfo,
  LogEntry as BackendLogEntry,
} from "../../bindings/warp/gui/models.js";
import { Stats as BackendStats } from "../../bindings/warp/route/models.js";

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

export function fromCounters(v: BackendStats): ProxyCounters {
  return {
    proxy: v.proxy,
    direct: v.direct,
    miss: v.miss,
    rejected: v.rejected,
  };
}

export function fromStatus(v: BackendStatus): AppStatus {
  return {
    running: v.state === "running",
    listening: v.listen_addr ?? "127.0.0.1:40000",
    startedAt: v.start_time,
    error: v.last_error,
    registered: v.registered,
    isAndroid: v.is_android,
    initDone: v.init_done,
    sysProxyOn: v.sys_proxy_on,
    counters: fromCounters(v.stats),
    registration: fromRegistration(v.registration),
  };
}

export function fromRegistration(
  v: BackendRegistrationInfo | null | undefined,
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

export function fromConfig(v: BackendConfig): AppConfig {
  const theme = v.theme_mode;
  return {
    listen: v.listen_addr,
    rulesPath: v.rules_path,
    geoDir: v.geo_dir,
    geoRepo: v.geo_repo,
    autoUpdateDays: v.geo_auto_update_days,
    systemProxy: v.enable_system_proxy,
    allowUDP: v.allow_udp,
    downloadProxy: v.download_proxy,
    themeMode:
      theme === "light" || theme === "dark" || theme === "system"
        ? theme
        : "system",
  };
}

export function fromGeo(v: BackendGeoInfo): GeoInfo {
  return {
    geositePath: v.geosite_path,
    geoipPath: v.geoip_path,
    geositeUpdated: v.geosite_updated,
    geoipUpdated: v.geoip_updated,
    repository: v.repository,
    baseURL: v.base_url,
    autoUpdateDays: v.auto_update_days,
    lastChecked: v.last_checked,
  };
}

const LEVELS: LogLevel[] = ["debug", "info", "warn", "error"];

export function fromLogs(v: BackendLogEntry[]): LogEntry[] {
  const out: LogEntry[] = [];
  for (const raw of v) {
    const lv = (raw.level ?? "info").toLowerCase();
    out.push({
      time: raw.time ?? "",
      level: (LEVELS.includes(lv as LogLevel) ? lv : "info") as LogLevel,
      msg: raw.msg ?? "",
    });
  }
  return out;
}

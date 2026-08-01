/**
 * Navigation model shared by the sidebar (md+) and the bottom nav (<md).
 * Pure data — unit-testable without jsdom.
 */

import { Activity, FileText, Globe, Radar, Settings, ScrollText } from "lucide-react";

export type PageKey = "status" | "rules" | "geo" | "scan" | "settings" | "logs";

export const NAV: { key: PageKey; label: string; icon: typeof Activity }[] = [
  { key: "status", label: "状态", icon: Activity },
  { key: "rules", label: "规则", icon: FileText },
  { key: "geo", label: "GEO", icon: Globe },
  { key: "scan", label: "扫描", icon: Radar },
  { key: "settings", label: "设置", icon: Settings },
  { key: "logs", label: "日志", icon: ScrollText },
];

export const TITLES: Record<PageKey, string> = {
  status: "状态",
  rules: "路由规则",
  geo: "GEO 数据库",
  scan: "边缘扫描",
  settings: "设置",
  logs: "运行日志",
};

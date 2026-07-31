import { useEffect, useState } from "react";
import {
  Activity,
  FileText,
  Globe,
  Moon,
  PanelLeftClose,
  PanelRightClose,
  Settings,
  Sun,
  ScrollText,
} from "lucide-react";
import StatusPage from "./pages/StatusPage";
import RulesPage from "./pages/RulesPage";
import GeoPage from "./pages/GeoPage";
import SettingsPage from "./pages/SettingsPage";
import LogsPage from "./pages/LogsPage";

type PageKey = "status" | "rules" | "geo" | "settings" | "logs";

const NAV: { key: PageKey; label: string; icon: typeof Activity }[] = [
  { key: "status", label: "状态", icon: Activity },
  { key: "rules", label: "规则", icon: FileText },
  { key: "geo", label: "GEO", icon: Globe },
  { key: "settings", label: "设置", icon: Settings },
  { key: "logs", label: "日志", icon: ScrollText },
];

const TITLES: Record<PageKey, string> = {
  status: "状态",
  rules: "路由规则",
  geo: "GEO 数据库",
  settings: "设置",
  logs: "运行日志",
};

function useDarkMode(): [boolean, () => void] {
  const [dark, setDark] = useState<boolean>(() => {
    const saved = localStorage.getItem("warpgo-dark");
    if (saved !== null) return saved === "1";
    return window.matchMedia("(prefers-color-scheme: dark)").matches;
  });
  useEffect(() => {
    document.documentElement.classList.toggle("dark", dark);
    localStorage.setItem("warpgo-dark", dark ? "1" : "0");
  }, [dark]);
  return [dark, () => setDark((d) => !d)];
}

export default function App() {
  const [page, setPage] = useState<PageKey>("status");
  const [collapsed, setCollapsed] = useState(false);
  const [dark, toggleDark] = useDarkMode();

  return (
    <div className="flex h-full bg-slate-100 text-slate-900 dark:bg-slate-950 dark:text-slate-100">
      {/* Sidebar */}
      <aside
        className={`flex shrink-0 flex-col border-r border-slate-200 bg-white transition-all dark:border-slate-800 dark:bg-slate-900 ${
          collapsed ? "w-14" : "w-52"
        }`}
      >
        <div className="flex items-center gap-2.5 px-4 py-4">
          <img
            src="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'%3E%3Crect width='32' height='32' rx='7' fill='%23f26432'/%3E%3Cpath d='M7 23 L12 9 L16 18 L20 9 L25 23' stroke='white' stroke-width='3.5' fill='none' stroke-linecap='round' stroke-linejoin='round'/%3E%3C/svg%3E"
            alt=""
            className="h-7 w-7 shrink-0"
          />
          {!collapsed && (
            <div className="min-w-0">
              <p className="truncate text-sm font-semibold">warp-go GUI</p>
              <p className="truncate text-[10px] text-slate-400">Cloudflare WARP</p>
            </div>
          )}
        </div>

        <nav className="flex-1 space-y-1 px-2 py-2">
          {NAV.map(({ key, label, icon: Icon }) => (
            <button
              key={key}
              onClick={() => setPage(key)}
              title={label}
              className={`flex w-full items-center gap-2.5 rounded-lg px-3 py-2 text-sm transition-colors ${
                page === key
                  ? "bg-orange-500/10 font-medium text-orange-600 dark:text-orange-400"
                  : "text-slate-600 hover:bg-slate-100 dark:text-slate-300 dark:hover:bg-slate-800"
              }`}
            >
              <Icon className="h-4.5 w-4.5 shrink-0" />
              {!collapsed && <span className="truncate">{label}</span>}
            </button>
          ))}
        </nav>

        <div className="flex items-center gap-1 border-t border-slate-200 p-2 dark:border-slate-800">
          <button
            onClick={toggleDark}
            title={dark ? "切换到浅色模式" : "切换到深色模式"}
            className="flex flex-1 items-center justify-center gap-2 rounded-lg px-3 py-2 text-sm text-slate-600 hover:bg-slate-100 dark:text-slate-300 dark:hover:bg-slate-800"
          >
            {dark ? <Sun className="h-4 w-4" /> : <Moon className="h-4 w-4" />}
            {!collapsed && <span>{dark ? "浅色" : "深色"}</span>}
          </button>
          <button
            onClick={() => setCollapsed((c) => !c)}
            title={collapsed ? "展开侧边栏" : "收起侧边栏"}
            aria-label={collapsed ? "展开侧边栏" : "收起侧边栏"}
            className="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg text-slate-500 transition-colors hover:bg-slate-100 hover:text-slate-700 dark:text-slate-400 dark:hover:bg-slate-800 dark:hover:text-slate-200"
          >
            {collapsed ? (
              <PanelRightClose className="h-5 w-5" />
            ) : (
              <PanelLeftClose className="h-5 w-5" />
            )}
          </button>
        </div>
      </aside>

      {/* Main */}
      <main className="flex min-w-0 flex-1 flex-col">
        <header className="flex items-center justify-between border-b border-slate-200 bg-white/60 px-6 py-3 backdrop-blur dark:border-slate-800 dark:bg-slate-900/60">
          <h1 className="text-base font-semibold">{TITLES[page]}</h1>
          <span className="text-xs text-slate-400">
            MASQUE over QUIC · SOCKS5 代理
          </span>
        </header>
        <div className="flex-1 overflow-y-auto p-6">
          <div className="mx-auto max-w-5xl">
            {page === "status" && <StatusPage />}
            {page === "rules" && <RulesPage />}
            {page === "geo" && <GeoPage />}
            {page === "settings" && <SettingsPage />}
            {page === "logs" && <LogsPage />}
          </div>
        </div>
      </main>
    </div>
  );
}

import { useCallback, useEffect, useMemo, useState, ReactNode } from "react";
import { Check, ChevronDown, Search, X } from "lucide-react";
import { listInstalledApps } from "../lib/api";
import type { InstalledApp } from "../lib/types";
import { Button, inputCls } from "./ui";

/**
 * 分应用代理的应用选择器（底部弹层）。
 *
 * 数据源：Java 侧 listInstalledApps()（只列声明 INTERNET 权限的应用）。
 * 分组：已选置顶 → 第三方应用 → 系统应用（默认折叠）。
 * 壳自身包名（selfPackage）已被上层过滤，列表里不会出现。
 * 搜索按名称/包名实时过滤。
 */
export function PerAppPicker({
  open,
  selected,
  selfPackage,
  onClose,
  onConfirm,
}: {
  open: boolean;
  /** 当前已选的包名集合（打开时初始化勾选态）。 */
  selected: string[];
  /** 壳自身包名，从列表剔除（防路由死锁）。 */
  selfPackage: string;
  onClose: () => void;
  onConfirm: (packages: string[]) => void;
}) {
  const [apps, setApps] = useState<InstalledApp[]>([]);
  const [loaded, setLoaded] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [query, setQuery] = useState("");
  const [picked, setPicked] = useState<Set<string>>(new Set(selected));
  const [showSystem, setShowSystem] = useState(false);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const list = await listInstalledApps();
      setApps(list.filter((a) => a.package !== selfPackage));
      setLoaded(true);
    } catch (e) {
      setError(String(e));
    } finally {
      setLoading(false);
    }
  }, [selfPackage]);

  useEffect(() => {
    if (open) {
      setPicked(new Set(selected));
      setQuery("");
      setShowSystem(false);
      void load();
    }
  }, [open, selected, load]);

  const toggle = (pkg: string) => {
    setPicked((prev) => {
      const next = new Set(prev);
      if (next.has(pkg)) next.delete(pkg);
      else next.add(pkg);
      return next;
    });
  };

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    return apps.filter(
      (a) =>
        q === "" ||
        (a.label || "").toLowerCase().includes(q) ||
        a.package.toLowerCase().includes(q)
    );
  }, [apps, query]);

  const selectedApps = useMemo(
    () => filtered.filter((a) => picked.has(a.package)),
    [filtered, picked]
  );
  const thirdParty = useMemo(
    () => filtered.filter((a) => !a.system && !picked.has(a.package)),
    [filtered, picked]
  );
  const systemApps = useMemo(
    () => filtered.filter((a) => a.system && !picked.has(a.package)),
    [filtered, picked]
  );

  if (!open) return null;

  const renderRow = (a: InstalledApp) => (
    <Row
      key={a.package}
      label={a.label || a.package}
      pkg={a.package}
      checked={picked.has(a.package)}
      onToggle={toggle}
    />
  );

  return (
    <div className="fixed inset-0 z-50 flex items-end justify-center bg-black/50 sm:items-center">
      <div className="flex max-h-[85vh] w-full max-w-md flex-col rounded-t-2xl bg-white shadow-xl dark:bg-slate-900 sm:rounded-2xl">
        <header className="flex items-center justify-between border-b border-slate-100 px-5 py-3 dark:border-slate-800">
          <h3 className="text-sm font-semibold text-slate-800 dark:text-slate-200">选择应用</h3>
          <button
            type="button"
            onClick={onClose}
            aria-label="关闭"
            className="rounded-md p-1 text-slate-500 hover:bg-slate-100 dark:text-slate-400 dark:hover:bg-slate-800"
          >
            <X className="h-4 w-4" />
          </button>
        </header>

        <div className="border-b border-slate-100 px-5 py-3 dark:border-slate-800">
          <div className="relative">
            <Search className="absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-slate-400" />
            <input
              className={`${inputCls} pl-9`}
              placeholder="搜索应用名/包名"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
            />
          </div>
          <p className="mt-2 text-xs text-slate-500 dark:text-slate-400">
            已选 {picked.size} 个
            {picked.size > 0 && (
              <button
                type="button"
                onClick={() => setPicked(new Set())}
                className="ml-2 text-orange-600 underline dark:text-orange-400"
              >
                清空
              </button>
            )}
          </p>
        </div>

        <div className="flex-1 overflow-y-auto px-2 py-2">
          {loading && (
            <p className="px-3 py-4 text-center text-sm text-slate-500 dark:text-slate-400">加载中…</p>
          )}
          {error && (
            <p className="px-3 py-4 text-center text-sm text-red-600 dark:text-red-400">
              加载失败：{error}
              <button type="button" onClick={load} className="ml-2 text-orange-600 underline dark:text-orange-400">
                重试
              </button>
            </p>
          )}
          {loaded && !error && filtered.length === 0 && (
            <p className="px-3 py-4 text-center text-sm text-slate-500 dark:text-slate-400">未找到应用</p>
          )}

          {selectedApps.length > 0 && (
            <Group label={`已选 (${selectedApps.length})`}>{selectedApps.map(renderRow)}</Group>
          )}
          {thirdParty.length > 0 && (
            <Group label="应用">{thirdParty.map(renderRow)}</Group>
          )}
          {systemApps.length > 0 && (
            <Group
              label={`系统应用 (${systemApps.length})`}
              collapsible
              open={showSystem}
              onToggle={() => setShowSystem((v) => !v)}
            >
              {systemApps.map(renderRow)}
            </Group>
          )}
        </div>

        <footer className="flex flex-col gap-2 border-t border-slate-100 px-5 py-3 dark:border-slate-800">
          <Button className="w-full" onClick={() => onConfirm(Array.from(picked))}>
            确定（已选 {picked.size} 个）
          </Button>
        </footer>
      </div>
    </div>
  );
}

function Group({
  label,
  children,
  collapsible = false,
  open = true,
  onToggle,
}: {
  label: string;
  children: ReactNode;
  collapsible?: boolean;
  open?: boolean;
  onToggle?: () => void;
}) {
  return (
    <div className="mb-1">
      {collapsible ? (
        <button
          type="button"
          onClick={onToggle}
          className="flex w-full items-center justify-between px-3 py-1.5 text-xs font-medium text-slate-500 dark:text-slate-400"
        >
          {label}
          <ChevronDown
            className={`h-3.5 w-3.5 transition-transform ${open ? "rotate-180" : ""}`}
          />
        </button>
      ) : (
        <p className="px-3 py-1.5 text-xs font-medium text-slate-500 dark:text-slate-400">{label}</p>
      )}
      {(!collapsible || open) && <div>{children}</div>}
    </div>
  );
}

function Row({
  label,
  pkg,
  checked,
  onToggle,
}: {
  label: string;
  pkg: string;
  checked: boolean;
  onToggle: (pkg: string) => void;
}) {
  return (
    <button
      type="button"
      onClick={() => onToggle(pkg)}
      className="flex w-full items-center gap-3 rounded-lg px-3 py-2 text-left hover:bg-slate-50 dark:hover:bg-slate-800"
    >
      <span
        className={`flex h-5 w-5 shrink-0 items-center justify-center rounded border ${
          checked
            ? "border-orange-500 bg-orange-500 text-white"
            : "border-slate-300 dark:border-slate-600"
        }`}
      >
        {checked && <Check className="h-3.5 w-3.5" />}
      </span>
      <span className="min-w-0 flex-1">
        <span className="block truncate text-sm text-slate-800 dark:text-slate-200">{label}</span>
        <span className="block truncate text-xs text-slate-400">{pkg}</span>
      </span>
    </button>
  );
}

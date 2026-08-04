/**
 * React hook for tri-state theme (light / dark / system) backed by the
 * theme-mode library in ./theme.ts.
 *
 * - Persists the mode via saveMode() and applies the resolved .dark class.
 * - Resolves the OS dark preference through the Wails runtime bridge
 *   (System.IsDarkMode()); while that promise is pending, or when it fails
 *   (e.g. running in a plain browser during dev), window.matchMedia stands in.
 * - Auto-switches on OS theme changes via Wails runtime events on every
 *   platform (common / windows / linux / android / ios), plus a matchMedia
 *   change listener as the browser fallback.
 */

import { useEffect, useState } from "react";
import { Events, System } from "@wailsio/runtime";
import {
  applyDarkClass,
  loadModeFromConfig,
  resolveDark,
  saveMode,
  type ThemeMode,
} from "./theme";
import { saveConfigPartial } from "./api";

/** Wails runtime theme-change event names, one per platform plus "common". */
export const THEME_EVENT_NAMES: readonly string[] = [
  "common:ThemeChanged",
  "windows:SystemThemeChanged",
  "linux:SystemThemeChanged",
  "android:ThemeChanged",
  "ios:ThemeChanged",
];

/**
 * Unwraps the payload from a Wails runtime event callback argument. The typed
 * callback receives a WailsEvent object ({ name, data }); some runtimes may
 * pass the bare payload instead, so anything without a `data` field is
 * returned unchanged.
 */
export function extractEventData(ev: unknown): unknown {
  if (ev && typeof ev === "object" && "data" in ev) {
    return (ev as { data: unknown }).data;
  }
  return ev;
}

/**
 * Extracts an explicit dark-mode flag from an event payload, or null when the
 * payload carries no usable isDarkMode value. Handles Android's JSON-string
 * payloads (e.g. '{"isDarkMode":true}') and plain objects. Payload shapes on
 * other platforms vary — callers fall back to re-querying System.IsDarkMode().
 */
export function extractIsDark(payload: unknown): boolean | null {
  let data = payload;
  if (typeof data === "string") {
    try {
      data = JSON.parse(data);
    } catch {
      return null;
    }
  }
  if (data && typeof data === "object" && "isDarkMode" in data) {
    const v = (data as { isDarkMode: unknown }).isDarkMode;
    if (typeof v === "boolean") {
      return v;
    }
  }
  return null;
}

/** Current prefers-color-scheme value, or false outside a DOM environment. */
function matchMediaDark(): boolean {
  if (typeof window === "undefined") {
    return false;
  }
  return window.matchMedia("(prefers-color-scheme: dark)").matches;
}

/** Queries the OS dark preference through the Wails bridge, ignoring failures. */
function querySystemDark(setDark: (dark: boolean) => void): void {
  System.IsDarkMode().then(setDark).catch(() => {
    /* Bridge unavailable (plain browser) — matchMedia stays authoritative. */
  });
}

export function useTheme(initialConfig?: { themeMode?: string } | null): {
  mode: ThemeMode;
  systemDark: boolean;
  setMode: (mode: ThemeMode) => void;
  setModeFromConfig: (config: { themeMode?: string } | null) => void;
} {
  const [mode, setModeState] = useState<ThemeMode>(() => loadModeFromConfig(initialConfig ?? null));
  // Seed with the browser preference; the Wails bridge overwrites it once it
  // resolves. In a plain browser the bridge never resolves, so the matchMedia
  // value stays authoritative.
  const [systemDark, setSystemDark] = useState<boolean>(() => matchMediaDark());

  // Resolve the real OS preference through the runtime bridge on mount.
  // Android: React mount 时 bridge 可能未就绪，System.IsDarkMode() 返回
  // false → 首帧误用浅色（用户反馈"默认白天模式"）。延迟重查一次，
  // 等 bridge 就绪后拿真实值。
  useEffect(() => {
    querySystemDark(setSystemDark);
    const t = setTimeout(() => querySystemDark(setSystemDark), 300);
    return () => clearTimeout(t);
  }, []);

  // Apply the resolved theme class and persist the choice.
  useEffect(() => {
    applyDarkClass(resolveDark(mode, systemDark));
    saveMode(mode);
    // Also persist to Go config.json
    saveConfigPartial({ themeMode: mode }).catch((e: Error) => {
      console.warn("保存主题到配置失败:", e);
    });
  }, [mode, systemDark]);

  const setMode = (newMode: ThemeMode) => {
    setModeState(newMode);
  };

  const setModeFromConfig = (config: { themeMode?: string } | null) => {
    const newMode = loadModeFromConfig(config);
    setModeState(newMode);
  };

  // Wails OS theme-change events (all platforms). The callback receives a
  // WailsEvent object; extractEventData unwraps it defensively.
  useEffect(() => {
    const offs = THEME_EVENT_NAMES.map((name) =>
      Events.On(name, (ev: unknown) => {
        const dark = extractIsDark(extractEventData(ev));
        if (dark !== null) {
          setSystemDark(dark);
        } else {
          // Unknown payload shape — ask the bridge for the current state.
          querySystemDark(setSystemDark);
        }
      }),
    );
    return () => { offs.forEach((off) => { off(); }); };
  }, []);

  // Browser fallback: keep following prefers-color-scheme changes.
  useEffect(() => {
    if (typeof window === "undefined") {
      return;
    }
    const mq = window.matchMedia("(prefers-color-scheme: dark)");
    const onChange = (e: MediaQueryListEvent) => setSystemDark(e.matches);
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, []);

  return { mode, systemDark, setMode, setModeFromConfig };
}

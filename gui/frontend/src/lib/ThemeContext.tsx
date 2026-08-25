/**
 * Theme context: mounts useTheme() exactly once at the App root so the
 * persisted theme loads and applies on startup — not just when the settings
 * page opens (the v0.5.30 regression: useTheme() lived only in SettingsPage,
 * so a fresh launch rendered light until the user visited settings).
 *
 * Pages consume the shared instance via useThemeContext(); creating another
 * useTheme() instance inside a page would fork the state (root would not see
 * settings-page changes).
 */

import { createContext, useContext, type ReactNode } from "react";
import { useTheme } from "./useTheme";
import type { ThemeMode } from "./theme";

type ThemeContextValue = {
  mode: ThemeMode;
  systemDark: boolean;
  setMode: (mode: ThemeMode) => void;
  setModeFromConfig: (config: { themeMode?: string } | null) => void;
};

const ThemeContext = createContext<ThemeContextValue | null>(null);

export function ThemeProvider({ children }: { children: ReactNode }) {
  const theme = useTheme();
  return <ThemeContext.Provider value={theme}>{children}</ThemeContext.Provider>;
}

/** Accesses the app-root theme instance. Must be used under <ThemeProvider>. */
export function useThemeContext(): ThemeContextValue {
  const ctx = useContext(ThemeContext);
  if (!ctx) {
    throw new Error("useThemeContext 必须在 <ThemeProvider> 内使用");
  }
  return ctx;
}

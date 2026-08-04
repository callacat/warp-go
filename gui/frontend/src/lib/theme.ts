/**
 * Theme-mode library (light / dark / system).
 *
 * Pure and storage-injectable so it can be unit-tested without jsdom or a
 * browser. The DOM-touching `applyDarkClass` is a thin wrapper that stays out
 * of the unit tests (guarded for non-DOM environments).
 */

export type ThemeMode = "light" | "dark" | "system";

/** Key for the persisted theme choice. */
export const THEME_KEY = "warpgo-theme";

/** Legacy key ("1"/"0" = dark/light) migrated to THEME_KEY on first load. */
export const LEGACY_THEME_KEY = "warpgo-dark";

const MODES: readonly ThemeMode[] = ["light", "dark", "system"];

/** Returns `v` when it is a valid ThemeMode, otherwise the default "system". */
export function normalizeMode(v: string | null): ThemeMode {
  return MODES.includes(v as ThemeMode) ? (v as ThemeMode) : "system";
}

/**
 * Reads the persisted mode. When the new key is absent, migrates the legacy
 * key ("1" → "dark", "0" → "light"), removes it, and writes the migrated value
 * under THEME_KEY. Falls back to "system" when nothing is stored.
 */
export function loadMode(
  storage: Pick<Storage, "getItem" | "setItem" | "removeItem"> = localStorage,
): ThemeMode {
  const stored = storage.getItem(THEME_KEY);
  if (stored !== null) {
    return normalizeMode(stored);
  }

  const legacy = storage.getItem(LEGACY_THEME_KEY);
  if (legacy !== null) {
    const mode: ThemeMode = legacy === "1" ? "dark" : legacy === "0" ? "light" : "system";
    storage.removeItem(LEGACY_THEME_KEY);
    saveMode(mode, storage);
    return mode;
  }

  return "system";
}

/** Persists the theme choice under THEME_KEY. */
export function saveMode(
  mode: ThemeMode,
  storage: Pick<Storage, "setItem"> = localStorage,
): void {
  storage.setItem(THEME_KEY, mode);
}

/**
 * Resolves the effective dark/light decision: explicit modes win, "system"
 * follows the OS preference.
 */
export function resolveDark(mode: ThemeMode, systemDark: boolean): boolean {
  switch (mode) {
    case "light":
      return false;
    case "dark":
      return true;
    case "system":
      return systemDark;
  }
}

/** Toggles the "dark" class on <html>. No-op outside a DOM environment. */
export function applyDarkClass(dark: boolean): void {
  if (typeof document === "undefined") {
    return;
  }
  document.documentElement.classList.toggle("dark", dark);
}

/** Loads theme mode from Go config (camelCase from snake_case JSON). */
export function loadModeFromConfig(config: { themeMode?: string } | null): ThemeMode {
  if (config?.themeMode) {
    return normalizeMode(config.themeMode);
  }
  return "system";
}

/** Returns payload to send to Go SaveConfig for theme mode. */
export function saveModeToConfig(mode: ThemeMode): { theme_mode: string } {
  return { theme_mode: mode };
}

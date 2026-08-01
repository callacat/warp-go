import { describe, it, expect } from "vitest";
import {
  normalizeMode,
  loadMode,
  saveMode,
  resolveDark,
  THEME_KEY,
  LEGACY_THEME_KEY,
} from "./theme";

/** Minimal in-memory Storage stub — no jsdom needed for these pure functions. */
class MemStorage implements Pick<Storage, "getItem" | "setItem" | "removeItem"> {
  m = new Map<string, string>();
  getItem(k: string): string | null {
    return this.m.get(k) ?? null;
  }
  setItem(k: string, v: string): void {
    this.m.set(k, v);
  }
  removeItem(k: string): void {
    this.m.delete(k);
  }
}

describe("normalizeMode", () => {
  it("round-trips every valid ThemeMode", () => {
    for (const v of ["light", "dark", "system"] as const) {
      expect(normalizeMode(v)).toBe(v);
    }
  });

  it("falls back to 'system' for null", () => {
    expect(normalizeMode(null)).toBe("system");
  });

  it("falls back to 'system' for bogus values", () => {
    expect(normalizeMode("blue")).toBe("system");
    expect(normalizeMode("")).toBe("system");
    expect(normalizeMode("DARK")).toBe("system");
  });
});

describe("loadMode", () => {
  it("returns 'system' when no keys are present", () => {
    const s = new MemStorage();
    expect(loadMode(s)).toBe("system");
  });

  it("migrates legacy '1' → 'dark'", () => {
    const s = new MemStorage();
    s.setItem(LEGACY_THEME_KEY, "1");
    expect(loadMode(s)).toBe("dark");
  });

  it("migrates legacy '0' → 'light'", () => {
    const s = new MemStorage();
    s.setItem(LEGACY_THEME_KEY, "0");
    expect(loadMode(s)).toBe("light");
  });

  it("removes the legacy key after migration", () => {
    const s = new MemStorage();
    s.setItem(LEGACY_THEME_KEY, "1");
    loadMode(s);
    expect(s.getItem(LEGACY_THEME_KEY)).toBeNull();
  });

  it("writes the migrated value under THEME_KEY", () => {
    const s = new MemStorage();
    s.setItem(LEGACY_THEME_KEY, "1");
    loadMode(s);
    expect(s.getItem(THEME_KEY)).toBe("dark");
  });

  it("prefers the new key over the legacy key", () => {
    const s = new MemStorage();
    s.setItem(THEME_KEY, "light");
    s.setItem(LEGACY_THEME_KEY, "1");
    expect(loadMode(s)).toBe("light");
  });

  it("ignores a bogus value under THEME_KEY", () => {
    const s = new MemStorage();
    s.setItem(THEME_KEY, "purple");
    expect(loadMode(s)).toBe("system");
  });
});

describe("saveMode", () => {
  it("round-trips a saved mode through loadMode", () => {
    for (const mode of ["light", "dark", "system"] as const) {
      const s = new MemStorage();
      saveMode(mode, s);
      expect(loadMode(s)).toBe(mode);
    }
  });
});

describe("resolveDark", () => {
  it("resolves explicit modes regardless of system preference", () => {
    expect(resolveDark("light", true)).toBe(false);
    expect(resolveDark("dark", false)).toBe(true);
  });

  it("follows the system preference for 'system'", () => {
    expect(resolveDark("system", true)).toBe(true);
    expect(resolveDark("system", false)).toBe(false);
  });
});

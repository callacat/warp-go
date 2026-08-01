import { describe, it, expect } from "vitest";
import { NAV, TITLES } from "./nav";
import type { PageKey } from "./nav";

const EXPECTED_ORDER: PageKey[] = ["status", "rules", "geo", "scan", "settings", "logs"];

describe("NAV", () => {
  it("has exactly 6 entries", () => {
    expect(NAV).toHaveLength(6);
  });

  it("has unique keys", () => {
    const keys = NAV.map((n) => n.key);
    expect(new Set(keys).size).toBe(keys.length);
  });

  it("keeps the fixed order: status, rules, geo, scan, settings, logs", () => {
    expect(NAV.map((n) => n.key)).toEqual(EXPECTED_ORDER);
  });

  it("every entry has a non-empty label", () => {
    for (const entry of NAV) {
      expect(entry.label.length).toBeGreaterThan(0);
    }
  });
});

describe("TITLES", () => {
  it("covers exactly every NAV key", () => {
    expect(Object.keys(TITLES).sort()).toEqual(EXPECTED_ORDER.slice().sort());
  });

  it("has one title per NAV entry", () => {
    expect(Object.keys(TITLES).length).toBe(NAV.length);
    for (const { key } of NAV) {
      expect(key in TITLES).toBe(true);
    }
  });

  it("every title value is non-empty", () => {
    for (const key of Object.keys(TITLES)) {
      expect(TITLES[key as PageKey].length).toBeGreaterThan(0);
    }
  });
});

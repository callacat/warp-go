import { describe, expect, it } from "vitest";
import { EDGE_PICKING_DISABLED_HINT, frontProxyEnabled } from "./frontProxy";

describe("frontProxyEnabled", () => {
  it("reads the enabled flag from a config object", () => {
    expect(frontProxyEnabled({ frontProxy: { enabled: true } })).toBe(true);
    expect(frontProxyEnabled({ frontProxy: { enabled: false } })).toBe(false);
  });

  it("treats a missing front_proxy block as disabled", () => {
    // 老配置（升级场景）没有 front_proxy 字段：不能因此把扫描/应用置灰
    expect(frontProxyEnabled({})).toBe(false);
    expect(frontProxyEnabled({ frontProxy: {} })).toBe(false);
    expect(frontProxyEnabled(null)).toBe(false);
    expect(frontProxyEnabled(undefined)).toBe(false);
  });
});

describe("EDGE_PICKING_DISABLED_HINT", () => {
  it("explains the mutual exclusion instead of a bare disabled button", () => {
    expect(EDGE_PICKING_DISABLED_HINT).toContain("百度中转");
    expect(EDGE_PICKING_DISABLED_HINT).toContain("优选 IP");
  });
});

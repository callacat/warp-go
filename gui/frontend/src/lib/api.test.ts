import { describe, it, expect } from "vitest";
import { buildSaveConfigPayload } from "./api";
import { AppConfig, DEFAULT_FRONT_PROXY_TOKEN } from "./types";

function baseConfig(): AppConfig {
  return {
    listen: "127.0.0.1:40000",
    rulesPath: "rules.txt",
    geoDir: "geo",
    geoRepo: "MetaCubeX/meta-rules-dat",
    autoUpdateDays: 7,
    systemProxy: false,
    allowUDP: false,
    downloadProxy: "https://gh-proxy.org/",
    themeMode: "system",
    perAppMode: "off",
    perAppPackages: [],
    frontProxy: {
      enabled: true,
      server: "cloudnproxy.baidu.com:443",
      connect_host: "sptest.baidu.com",
      token: "",
      user_agent: "",
    },
  };
}

describe("buildSaveConfigPayload front_proxy token 预填语义", () => {
  it("frontProxy 整体缺失 → token 回填默认共享凭据", () => {
    const cfg = baseConfig();
    delete (cfg as { frontProxy?: unknown }).frontProxy;
    const payload = buildSaveConfigPayload(cfg);
    const fp = payload.front_proxy as Record<string, unknown>;
    expect(fp.token).toBe(DEFAULT_FRONT_PROXY_TOKEN);
    expect(fp.server).toBe("cloudnproxy.baidu.com:443");
    expect(fp.connect_host).toBe("sptest.baidu.com");
    expect(fp.enabled).toBe(false);
  });

  it("token 显式空串（存量 config.json 形态）→ 回填默认共享凭据，不落盘空 token", () => {
    const payload = buildSaveConfigPayload(baseConfig());
    const fp = payload.front_proxy as Record<string, unknown>;
    expect(fp.token).toBe(DEFAULT_FRONT_PROXY_TOKEN);
    expect(fp.enabled).toBe(true);
  });

  it("用户显式 token 非空 → 透传不被覆盖", () => {
    const cfg = baseConfig();
    if (cfg.frontProxy) cfg.frontProxy.token = "user-custom";
    const payload = buildSaveConfigPayload(cfg);
    const fp = payload.front_proxy as Record<string, unknown>;
    expect(fp.token).toBe("user-custom");
  });
});
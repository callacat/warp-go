import { describe, it, expect } from "vitest";
import { fromStatus, fromGeo } from "./types";

// 用后端 core.Status JSON 的真实形态（snake_case）验证前端映射。
// 后端序列化实测：{"state":"running","registration":{"id":"dev-123",
// "account":"acct","key_type":"curve25519","tunnel_type":"masque",
// "endpoint_v4":"1.2.3.4","endpoint_v6":"::1","endpoint_ports":[443],
// "assigned_ipv4":"172.16.0.2","assigned_ipv6":"2606::1"}}
const STATUS_JSON = {
  state: "running",
  registered: true,
  registration: {
    id: "dev-123",
    account: "acct",
    key_type: "curve25519",
    tunnel_type: "masque",
    endpoint_v4: "1.2.3.4",
    endpoint_v6: "::1",
    endpoint_ports: [443],
    assigned_ipv4: "172.16.0.2",
    assigned_ipv6: "2606::1",
  },
};

describe("fromStatus", () => {
  it("maps state to running flag", () => {
    expect(fromStatus(STATUS_JSON).running).toBe(true);
  });

  it("maps all registration fields", () => {
    const r = fromStatus(STATUS_JSON).registration;
    expect(r).not.toBeNull();
    expect(r?.id).toBe("dev-123");
    expect(r?.account).toBe("acct");
    expect(r?.keyType).toBe("curve25519");
    expect(r?.tunnelType).toBe("masque");
    expect(r?.endpointV4).toBe("1.2.3.4");
    expect(r?.endpointV6).toBe("::1");
    expect(r?.endpointPorts).toEqual([443]);
    expect(r?.assignedIPv4).toBe("172.16.0.2");
    expect(r?.assignedIPv6).toBe("2606::1");
  });

  it("state is empty when status lacks state field", () => {
    expect(fromStatus({}).running).toBe(false);
  });

  it("maps is_android flag", () => {
    expect(fromStatus({ is_android: true }).isAndroid).toBe(true);
    expect(fromStatus({ is_android: false }).isAndroid).toBe(false);
    expect(fromStatus({}).isAndroid).toBe(false);
  });

  it("maps init_done flag", () => {
    expect(fromStatus({ init_done: true }).initDone).toBe(true);
    expect(fromStatus({ init_done: false }).initDone).toBe(false);
    expect(fromStatus({}).initDone).toBe(false);
  });

  it("maps sys_proxy_on flag", () => {
    expect(fromStatus({ sys_proxy_on: true }).sysProxyOn).toBe(true);
    expect(fromStatus({ sys_proxy_on: false }).sysProxyOn).toBe(false);
    expect(fromStatus({}).sysProxyOn).toBe(false);
  });
});

describe("fromGeo", () => {
  const GEO_JSON = {
    geosite_path: "geo/geosite.dat",
    geoip_path: "geo/geoip-lite.dat",
    geosite_updated: "2026-08-02 10:00",
    geoip_updated: "2026-08-02 10:00",
    repository: "MetaCubeX/meta-rules-dat",
    base_url: "https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest",
    auto_update_days: 7,
    last_checked: "2026-08-02 10:00",
  };

  it("maps updated timestamps from snake_case", () => {
    const g = fromGeo(GEO_JSON);
    expect(g.geositeUpdated).toBe("2026-08-02 10:00");
    expect(g.geoipUpdated).toBe("2026-08-02 10:00");
    expect(g.lastChecked).toBe("2026-08-02 10:00");
    expect(g.repository).toBe("MetaCubeX/meta-rules-dat");
  });

  it("falls back to defaults when fields missing", () => {
    const g = fromGeo({});
    expect(g.geositeUpdated).toBeUndefined();
    expect(g.repository).toBe("MetaCubeX/meta-rules-dat");
  });
});

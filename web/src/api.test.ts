import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient } from "./api";

const jsonHeaders = { "Content-Type": "application/json" };

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("API response contracts", () => {
  it.each<[string, Record<string, string>]>([
    ["kuaifan", { account: "operator@example.com", password: "volatile-password" }],
    ["baidu", {}],
  ])("posts the declared credential map for %s", async (provider, fields) => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({}), { status: 200, headers: jsonHeaders }));
    vi.stubGlobal("fetch", fetchMock);

    await expect(new ApiClient().login(provider, fields)).resolves.toEqual({});

    expect(fetchMock.mock.calls[0][0]).toBe("/api/v1/auth/login");
    expect(fetchMock.mock.calls[0][1]).toMatchObject({ method: "POST", credentials: "same-origin", cache: "no-store" });
    expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toEqual({ provider, fields });
  });

  it("propagates invalid_login when the provider rejects the credential shape", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ title: "Invalid provider fields", status: 400, code: "invalid_login" }), { status: 400, headers: jsonHeaders }));
    vi.stubGlobal("fetch", fetchMock);

    await expect(new ApiClient().login("kuaifan", {})).rejects.toMatchObject({ problem: { status: 400, code: "invalid_login" } });
  });

  it("strips endpoint and exclusion fields from node list responses", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({
      nodes: [{
        id: "node-1",
        name: "Node 01",
        group: "Test group",
        provider: "Test provider",
        endpoint: "203.0.113.10:11000",
        excluded: true,
        health: "healthy",
        tcpLatencyMs: 42,
        udpHealth: "unavailable",
        eligible: true,
      }],
    }), { status: 200, headers: jsonHeaders }));
    vi.stubGlobal("fetch", fetchMock);

    const result = await new ApiClient().nodes();

    expect(result.nodes[0]).toMatchObject({
      id: "node-1",
      name: "Node 01",
      health: "healthy",
      tcpLatencyMs: 42,
    });
    expect(result.nodes[0]).not.toHaveProperty("endpoint");
    expect(result.nodes[0]).not.toHaveProperty("excluded");
  });

  it("loads URL-encoded node details with a no-store request", async () => {
    const details = {
      id: "node / 1",
      name: "Node 01",
      group: "Test group",
      provider: "Test provider",
      upstreamHost: "127.0.0.1",
      upstreamPort: 10808,
      socksAddress: "127.0.0.1:10808",
      socksUsername: "local-user",
      socksPassword: "local-password",
      health: "healthy",
      tcpLatencyMs: 42,
    };
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify(details), { status: 200, headers: jsonHeaders }));
    vi.stubGlobal("fetch", fetchMock);

    await expect(new ApiClient().nodeDetails("node / 1")).resolves.toEqual(details);

    expect(fetchMock.mock.calls[0][0]).toBe("/api/v1/nodes/node%20%2F%201/details");
    expect(fetchMock.mock.calls[0][1]).toMatchObject({ method: "GET", credentials: "same-origin", cache: "no-store" });
  });

  it("keeps account logout separate from the console lock", async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(null, { status: 204 }))
      .mockResolvedValueOnce(new Response(null, { status: 202 }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }))
      .mockResolvedValueOnce(new Response(null, { status: 202 }));
    vi.stubGlobal("fetch", fetchMock);
    const api = new ApiClient();

		await expect(api.logoutAccount("kuaifan")).resolves.toBeUndefined();
		await api.refresh("kuaifan");
    await expect(api.lockConsole()).resolves.toBeUndefined();
		await api.refresh("kuaifan");

    expect(fetchMock.mock.calls[0][0]).toBe("/api/v1/auth/logout");
    expect(fetchMock.mock.calls[0][1]).toMatchObject({ method: "POST", credentials: "same-origin", cache: "no-store" });
		expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toEqual({ provider: "kuaifan" });
    expect(fetchMock.mock.calls[1][1].headers).toEqual({ Accept: "application/json", "Content-Type": "application/json" });
		expect(JSON.parse(fetchMock.mock.calls[1][1].body)).toEqual({ provider: "kuaifan" });
    expect(fetchMock.mock.calls[2][0]).toBe("/api/v1/access/logout");
    expect(fetchMock.mock.calls[2][1].method).toBe("POST");
    expect(JSON.parse(fetchMock.mock.calls[2][1].body)).toEqual({});
    expect(fetchMock.mock.calls[3][1].headers).toEqual({ Accept: "application/json", "Content-Type": "application/json" });
  });

  it("accepts the empty 202 refresh acknowledgement", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 202 }));
    vi.stubGlobal("fetch", fetchMock);

		await expect(new ApiClient().refresh("kuaifan")).resolves.toBeUndefined();

    expect(fetchMock.mock.calls[0][0]).toBe("/api/v1/control/refresh");
    expect(fetchMock.mock.calls[0][1].method).toBe("POST");
		expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toEqual({ provider: "kuaifan" });
  });

  it("posts an empty JSON body to export diagnostics and returns the report", async () => {
    const report = JSON.stringify({ version: "0.1.0" });
    const fetchMock = vi.fn().mockResolvedValue(new Response(report, { status: 200, headers: jsonHeaders }));
    vi.stubGlobal("fetch", fetchMock);

    const blob = await new ApiClient().diagnostics();

    expect(await blob.text()).toBe(report);
    expect(fetchMock.mock.calls[0][0]).toBe("/api/v1/diagnostics/export");
    expect(fetchMock.mock.calls[0][1]).toMatchObject({ method: "POST", credentials: "same-origin", cache: "no-store" });
    expect(fetchMock.mock.calls[0][1].headers).toEqual({ Accept: "application/json", "Content-Type": "application/json" });
    expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toEqual({});
  });

  it("uses the smart proxy endpoints with JSON bodies for mutations", async () => {
    const status = { enabled: true, intervalMinutes: 60, running: false, targets: [], results: [] };
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify(status), { status: 200, headers: jsonHeaders }))
      .mockResolvedValueOnce(new Response(JSON.stringify(status), { status: 200, headers: jsonHeaders }))
      .mockResolvedValueOnce(new Response(null, { status: 202 }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ url: "socks5://u:p@127.0.0.1:10808" }), { status: 200, headers: jsonHeaders }));
    vi.stubGlobal("fetch", fetchMock);
    const api = new ApiClient();

    await expect(api.smartProxy()).resolves.toEqual(status);
    await expect(api.configureSmartProxy(true, 60)).resolves.toEqual(status);
    await expect(api.probeSmartProxy()).resolves.toBeUndefined();
    await expect(api.smartProxyDetails()).resolves.toMatchObject({ url: "socks5://u:p@127.0.0.1:10808" });

    expect(fetchMock.mock.calls.map(([path, init]) => [path, init.method])).toEqual([
      ["/api/v1/smart-proxy", "GET"],
      ["/api/v1/smart-proxy/config", "PUT"],
      ["/api/v1/smart-proxy/probe", "POST"],
      ["/api/v1/smart-proxy/details", "GET"],
    ]);
    expect(JSON.parse(fetchMock.mock.calls[1][1].body)).toEqual({ enabled: true, intervalMinutes: 60 });
    expect(fetchMock.mock.calls[1][1].headers).toEqual({ Accept: "application/json", "Content-Type": "application/json" });
    expect(JSON.parse(fetchMock.mock.calls[2][1].body)).toEqual({});
  });
});

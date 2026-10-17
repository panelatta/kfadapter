import { vi } from "vitest";
import { type ApiClient } from "../api";
import type { InboundChoices, InboundsStatus, NodeDetails, NodeRecord, SmartProxyStatus, StatusResponse } from "../types";

export const currentConsumerURL = "http://127.0.0.1:10809/sub/00112233-4455-8677-8899-aabbccddeeff";
/** A second capability URL, for tests where the subscription link changes. */
export const updatedConsumerURL = "http://127.0.0.1:10809/sub/ffeeddcc-bbaa-8988-8766-554433221100";

export const disabledSmartProxy = (): SmartProxyStatus => ({
  enabled: false, intervalMinutes: 30, running: false, candidateCount: 0,
  incomplete: false, targets: [], results: [],
});

export const inboundsStatus = (choices: InboundChoices = { socks5: true, snell: false, shadowsocks: false, socksTLS: false }): InboundsStatus => ({
  tlsFingerprint: "8bd0624728415a451f10ae35626a4c2978cde481413452b41323f467c2f8f705",
  inbounds: [
    { protocol: "socks5", port: 10808, enabled: choices.socks5, listening: choices.socks5 },
    { protocol: "snell", port: 10810, enabled: choices.snell, listening: choices.snell },
    { protocol: "shadowsocks", port: 10811, enabled: choices.shadowsocks, listening: choices.shadowsocks },
    { protocol: "socks5-tls", port: 10812, enabled: choices.socksTLS, listening: choices.socksTLS },
  ],
});

export const readyStatus = (): StatusResponse => ({
  state: "ready",
  version: "0.1.0",
  deployment: { mode: "container" },
  providers: ["kuaifan", "quickfox", "transocks", "baidu"],
  providerAuth: {
    kuaifan: { fields: [{ key: "account", label: "Email", secret: false }, { key: "password", label: "Password", secret: true }] },
    quickfox: { fields: [{ key: "account", label: "Email", secret: false }, { key: "password", label: "Password", secret: true }] },
    transocks: { fields: [{ key: "account", label: "Email or phone", secret: false }, { key: "password", label: "Password", secret: true }] },
    baidu: { fields: [] },
  },
	accounts: { kuaifan: { provider: "kuaifan", display: "u•••@example.com", tier: "VIP", subscriptionActive: true, subscriptionEndsAt: "2026-08-15T10:00:00Z" } },
	controlPlane: { lastRefreshAt: "2026-07-15T10:00:00Z" },
	dataPlane: { socksAddress: "127.0.0.1:10808", udpMode: "provider_transport" },
  nodes: { total: 3, eligible: 2, healthy: 1 },
  subscription: { active: true, nodeCount: 2 },
});

export const signedOutStatus = (): StatusResponse => ({
  ...readyStatus(),
  state: "signed_out",
	accounts: undefined,
  nodes: { total: 0, eligible: 0, healthy: 0 },
  subscription: { active: false, nodeCount: 0 },
});

export const nodes: NodeRecord[] = [
  { id: "n-east", name: "Shanghai 01", group: "East China", provider: "kuaifan", protocol: "kuaifan-wifiin", health: "healthy", tcpLatencyMs: 76, udpHealth: "unavailable", eligible: true },
  { id: "n-west", name: "Chengdu 02", group: "West China", provider: "kuaifan", protocol: "kuaifan-speedin", health: "degraded", tcpLatencyMs: 114, udpHealth: "unavailable", eligible: true },
];

export const chengduDetails: NodeDetails = {
  id: "n-west",
  name: "Chengdu 02",
  group: "West China",
  provider: "WIFIIN",
  upstreamHost: "127.0.0.1",
  upstreamPort: 10808,
  socksAddress: "127.0.0.1:10808",
  socksUsername: "n_AAECAwQFBgcICQoL",
  socksPassword: "10213243-5465-8768-899a-abbccddeeff0",
  health: "degraded",
  tcpLatencyMs: 114,
};

export function makeApi(initialStatus = readyStatus(), initialNodes = nodes): ApiClient & { statusValue: StatusResponse } {
  const state = {
    statusValue: initialStatus,
    nodeList: initialNodes,
    access: { initialized: true, authenticated: true },
  };
  return {
    get statusValue() { return state.statusValue; },
    set statusValue(next: StatusResponse) { state.statusValue = next; },
    accessStatus: vi.fn().mockImplementation(async () => state.access),
    setupAccess: vi.fn().mockImplementation(async () => { state.access = { ...state.access, initialized: true, authenticated: true }; return {}; }),
    loginAccess: vi.fn().mockImplementation(async () => { state.access = { ...state.access, authenticated: true }; return {}; }),
    changeAccessToken: vi.fn().mockResolvedValue({}),
    status: vi.fn().mockImplementation(async () => state.statusValue),
    nodes: vi.fn().mockImplementation(async () => ({ nodes: state.nodeList })),
    nodeDetails: vi.fn().mockResolvedValue(chengduDetails),
    smartProxy: vi.fn().mockImplementation(async () => disabledSmartProxy()),
    configureSmartProxy: vi.fn().mockImplementation(async (enabled: boolean, intervalMinutes: number) => ({ enabled, intervalMinutes, running: false, targets: [], results: [] })),
    probeSmartProxy: vi.fn().mockResolvedValue(undefined),
    smartProxyDetails: vi.fn().mockResolvedValue({ url: "socks5://automatic:secret@127.0.0.1:10808" }),
    inbounds: vi.fn().mockImplementation(async () => inboundsStatus()),
    configureInbounds: vi.fn().mockImplementation(async (choices: InboundChoices) => inboundsStatus(choices)),
    diagnostics: vi.fn().mockResolvedValue(new Blob(["{}"], { type: "application/json" })),
    subscriptionURL: vi.fn().mockResolvedValue({ url: currentConsumerURL }),
    login: vi.fn().mockImplementation(async (provider: string) => { state.statusValue = readyStatus(); return state.statusValue.accounts?.[provider] ?? { provider, tier: "Standard", subscriptionActive: false }; }),
    logoutAccount: vi.fn().mockResolvedValue(undefined),
    lockConsole: vi.fn().mockResolvedValue(undefined),
    refresh: vi.fn().mockResolvedValue(undefined),
    probeNode: vi.fn().mockImplementation(async (nodeId: string) => ({ nodeId, health: "healthy", tcpLatencyMs: 84, probedAt: "2026-07-15T10:03:00Z" })),
    events: vi.fn().mockReturnValue(() => {}),
    } as unknown as ApiClient & { statusValue: StatusResponse };
}

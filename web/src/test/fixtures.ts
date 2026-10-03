import { vi } from "vitest";
import { type ApiClient } from "../api";
import type { NodeDetails, NodeRecord, StatusResponse } from "../types";

export const currentConsumerURL = "http://127.0.0.1:10809/sub/0123456789012345678901234567890123456789012";

export const readyStatus = (): StatusResponse => ({
  state: "ready",
  version: "0.1.0",
  deployment: { mode: "container" },
	providers: ["kuaifan", "quickfox"],
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
  { id: "n-east", name: "Shanghai 01", group: "East China", provider: "kuaifan", health: "healthy", tcpLatencyMs: 76, udpHealth: "unavailable", eligible: true },
  { id: "n-west", name: "Chengdu 02", group: "West China", provider: "kuaifan", health: "degraded", tcpLatencyMs: 114, udpHealth: "unavailable", eligible: true },
];

export const chengduDetails: NodeDetails = {
  id: "n-west",
  name: "Chengdu 02",
  group: "West China",
  provider: "WIFIIN",
  upstreamHost: "127.0.0.1",
  upstreamPort: 10808,
  socksAddress: "127.0.0.1:10808",
  socksUsername: "local-socks-user",
  socksPassword: "local-socks-password",
  health: "degraded",
  tcpLatencyMs: 114,
};

export function makeApi(initialStatus = readyStatus(), initialNodes = nodes): ApiClient & { statusValue: StatusResponse } {
  const state = {
    statusValue: initialStatus,
    nodeList: initialNodes,
    access: { initialized: true, authenticated: true, csrfToken: "csrf-test" },
  };
  return {
    get statusValue() { return state.statusValue; },
    set statusValue(next: StatusResponse) { state.statusValue = next; },
    setCsrfToken: vi.fn(),
    clearSession: vi.fn(),
    accessStatus: vi.fn().mockImplementation(async () => state.access),
    setupAccess: vi.fn().mockImplementation(async () => { state.access = { ...state.access, initialized: true, authenticated: true }; return { csrfToken: "csrf-test" }; }),
    loginAccess: vi.fn().mockImplementation(async () => { state.access = { ...state.access, authenticated: true }; return { csrfToken: "csrf-test" }; }),
    status: vi.fn().mockImplementation(async () => state.statusValue),
    nodes: vi.fn().mockImplementation(async () => ({ nodes: state.nodeList })),
    nodeDetails: vi.fn().mockResolvedValue(chengduDetails),
    smartProxy: vi.fn().mockResolvedValue({ enabled: false, intervalMinutes: 30, running: false, targets: [], results: [] }),
    configureSmartProxy: vi.fn().mockImplementation(async (enabled: boolean, intervalMinutes: number) => ({ enabled, intervalMinutes, running: false, targets: [], results: [] })),
    probeSmartProxy: vi.fn().mockResolvedValue(undefined),
    smartProxyDetails: vi.fn().mockResolvedValue({ url: "socks5://automatic:secret@127.0.0.1:10808" }),
    subscriptionURL: vi.fn().mockResolvedValue({ url: currentConsumerURL }),
    login: vi.fn().mockImplementation(async () => { state.statusValue = readyStatus(); return {}; }),
    logoutAccount: vi.fn().mockResolvedValue(undefined),
    lockConsole: vi.fn().mockResolvedValue(undefined),
    refresh: vi.fn().mockResolvedValue(undefined),
    probeNode: vi.fn().mockImplementation(async (nodeId: string) => ({ nodeId, health: "healthy", tcpLatencyMs: 84, probedAt: "2026-07-15T10:03:00Z" })),
    events: vi.fn().mockReturnValue(() => {}),
    } as unknown as ApiClient & { statusValue: StatusResponse };
}

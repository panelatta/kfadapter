import type { ProbeFailure } from "./features/nodes/health";

export type ServiceState =
  | "signed_out"
  | "authenticating"
  | "syncing"
  | "ready"
  | "degraded"
  | "expired"
  | "error";

export type NodeHealth = "healthy" | "degraded" | "unhealthy" | "unknown";

export type RefreshStatus = "ok" | "refresh_failing" | "needs_relogin";

export interface AccountSummary {
  provider: string;
  display?: string;
  tier: string;
  subscriptionActive: boolean;
  subscriptionEndsAt?: string;
  refreshStatus?: RefreshStatus;
  /** Redacted class of the latest failed refresh, e.g. "network". */
  refreshProblem?: string;
  lastRefreshAt?: string;
  nextRefreshAt?: string;
}

export interface ProviderAuthSpec {
  fields: { key: string; label: string; secret: boolean }[];
}

export interface StatusResponse {
  state: ServiceState;
  version: string;
  deployment: {
    mode: "container" | string;
    startedAt?: string;
  };
  providers: string[];
  /** Credential fields declared by each provider; an empty list denotes a public service. */
  providerAuth: Record<string, ProviderAuthSpec>;
  accounts?: Record<string, AccountSummary>;
  controlPlane: {
    lastRefreshAt?: string;
    nextRefreshAt?: string;
  };
  dataPlane: {
    socksAddress: string;
    udpMode: "provider_transport";
  };
  nodes: {
    total: number;
    eligible: number;
    healthy: number;
  };
  subscription: {
    active: boolean;
    nodeCount: number;
  };
}

export interface NodeRecord {
  id: string;
  name: string;
  group: string;
  provider: string;
  protocol: string;
  health: NodeHealth;
  tcpLatencyMs?: number;
  /** Why the last TCP probe failed; a key of the probe failure messages. */
  probeError?: ProbeFailure;
  udpHealth: "unavailable" | "healthy" | "unhealthy" | string;
  eligible: boolean;
}

export interface NodeDetails {
  id: string;
  name: string;
  group: string;
  provider: string;
  upstreamHost: string;
  upstreamPort: number;
  socksAddress: string;
  socksUsername: string;
  /** Installation-wide lowercase UUIDv8; shared by every node. */
  socksPassword: string;
  health: NodeHealth;
  tcpLatencyMs?: number;
}

export interface NodesResponse {
  nodes: NodeRecord[];
}

export interface SubscriptionMetadata {
  active: boolean;
  nodeCount: number;
}

export interface SubscriptionURLResponse {
  /** Absolute URL ending in /sub/<lowercase canonical UUIDv8>. */
  url: string;
}


export interface AccessStatusResponse {
  initialized: boolean;
  /** Only a browser on the adapter host itself may create the first token. */
  setupAllowed?: boolean;
  authenticated: boolean;
  expiresAt?: string;
}

export interface AccessSessionResponse {
  expiresAt?: string;
}

export interface AccessTokenChangeInput {
  current: string;
  next: string;
}

/** POST /auth/login returns the connected account (web.Account in Go). */
export type LoginResponse = AccountSummary;

export interface ProbeResult {
  nodeId: string;
  health: NodeHealth;
  tcpLatencyMs?: number;
  probedAt: string;
}

export type EventMessage =
  | { type: "state"; state: ServiceState }
  | { type: "refresh"; state: ServiceState; complete: boolean }
  | { type: "probe"; nodeId: string; health: NodeHealth; tcpLatencyMs?: number; probedAt: string };

export interface SmartProxyTarget {
  name: string;
  url: string;
}

export interface SmartProxyMeasurement {
  target: string;
  ok: boolean;
  latencyMs?: number;
  error?: string;
}

export interface SmartProxyResult {
  nodeId: string;
  name: string;
  provider: string;
  successes: number;
  latencyMs?: number;
  measurements: SmartProxyMeasurement[];
}

export interface SmartProxyStatus {
  enabled: boolean;
  intervalMinutes: number;
  running: boolean;
  candidateCount?: number;
  incomplete?: boolean;
  lastRunAt?: string;
  nextRunAt?: string;
  selectedNodeId?: string;
  selectedName?: string;
  targets: SmartProxyTarget[];
  results: SmartProxyResult[];
}

export interface SmartProxyDetails {
  socksAddress: string;
  socksUsername: string;
  socksPassword: string;
  url: string;
}

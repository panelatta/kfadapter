export type ServiceState =
  | "signed_out"
  | "authenticating"
  | "syncing"
  | "ready"
  | "degraded"
  | "expired"
  | "error";

export type NodeHealth = "healthy" | "degraded" | "unhealthy" | "unknown";

export interface AccountSummary {
  provider: string;
  display?: string;
  tier: string;
  subscriptionActive: boolean;
  subscriptionEndsAt?: string;
}

export interface StatusResponse {
  state: ServiceState;
  version: string;
  deployment: {
    mode: "container" | string;
    startedAt?: string;
  };
  providers: string[];
  accounts?: Record<string, AccountSummary>;
  controlPlane: {
    lastRefreshAt?: string;
    nextRefreshAt?: string;
  };
  dataPlane: {
    socksAddress: string;
    udpMode: "disabled_unverified" | string;
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
  health: NodeHealth;
  tcpLatencyMs?: number;
  probeError?: string;
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
  socksPassword: string;
  health: NodeHealth;
  tcpLatencyMs?: number;
}

export interface NodesResponse {
  nodes: NodeRecord[];
}

export interface SubscriptionURLResponse {
  url: string;
}


export interface AccessStatusResponse {
  initialized: boolean;
  /** Only a browser on the adapter host itself may create the first token. */
  setupAllowed?: boolean;
  authenticated: boolean;
  csrfToken?: string;
  expiresAt?: string;
}

export interface AccessSessionResponse {
  csrfToken?: string;
  expiresAt?: string;
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

export interface SmartProxyStatus {
  candidateCount?: number;
  incomplete?: boolean;
  enabled: boolean;
  intervalMinutes: number;
  running: boolean;
  lastRunAt?: string;
  nextRunAt?: string;
  selectedNodeId?: string;
  selectedName?: string;
  targets: { name: string; url: string }[];
  results: {
    nodeId: string; name: string; provider: string; successes: number; latencyMs?: number;
    measurements: { target: string; ok: boolean; latencyMs?: number; error?: string }[];
  }[];
}
export interface SmartProxyDetails {
  socksAddress: string;
  socksUsername: string;
  socksPassword: string;
  url: string;
}

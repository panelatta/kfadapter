import type {
  AccessSessionResponse,
  AccessStatusResponse,
  EventMessage,
  LoginResponse,
  NodeDetails,
  NodeHealth,
  NodesResponse,
  ProbeResult,
  ServiceState,
  StatusResponse,
  SmartProxyStatus,
  SmartProxyDetails,
  SubscriptionURLResponse,
} from "./types";

export interface ProblemDetails {
  type?: string;
  title: string;
  status: number;
  code: string;
  detail?: string;
}

export class ApiError extends Error {
  readonly problem: ProblemDetails;

  constructor(problem: ProblemDetails) {
    super(problem.detail || problem.title);
    this.name = "ApiError";
    this.problem = problem;
  }
}

const API_ROOT = "/api/v1";
const EVENT_SOURCE_CLOSED = 2;
const EVENT_RETRY_INITIAL_MS = 1_000;
const EVENT_RETRY_MAX_MS = 30_000;


async function asProblem(response: Response): Promise<ApiError> {
  let problem: Partial<ProblemDetails> = {};
  try {
    problem = (await response.json()) as Partial<ProblemDetails>;
  } catch {
    // The server may reject before it can issue a problem document.
  }
  return new ApiError({
    title: problem.title || "Request rejected",
    status: response.status,
    code: problem.code || `http_${response.status}`,
    detail: problem.detail,
    type: problem.type,
  });
}

const serviceStates: readonly ServiceState[] = ["signed_out", "authenticating", "syncing", "ready", "degraded", "expired", "error"];
const nodeHealths: readonly NodeHealth[] = ["healthy", "degraded", "unhealthy", "unknown"];

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function isNonnegativeSafeInteger(value: unknown): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= 0;
}

function parseNamedEvent(type: "state" | "refresh" | "probe", data: string): EventMessage | null {
  let payload: unknown;
  try {
    payload = JSON.parse(data);
  } catch {
    return null;
  }
  if (!isRecord(payload)) return null;

  if (type === "state") {
    if (typeof payload.state !== "string" || !serviceStates.includes(payload.state as ServiceState)) return null;
    return { type, state: payload.state as ServiceState };
  }
  if (type === "refresh") {
    if (typeof payload.state !== "string" || !serviceStates.includes(payload.state as ServiceState) || typeof payload.complete !== "boolean") return null;
    return { type, state: payload.state as ServiceState, complete: payload.complete };
  }
  if (type === "probe") {
    if (
      typeof payload.nodeId !== "string" || payload.nodeId.length === 0 ||
      typeof payload.health !== "string" || !nodeHealths.includes(payload.health as NodeHealth) ||
      typeof payload.probedAt !== "string" || payload.probedAt.length === 0 ||
      (payload.tcpLatencyMs !== undefined && !isNonnegativeSafeInteger(payload.tcpLatencyMs))
    ) return null;
    return payload.tcpLatencyMs === undefined
      ? { type, nodeId: payload.nodeId, health: payload.health as NodeHealth, probedAt: payload.probedAt }
      : { type, nodeId: payload.nodeId, health: payload.health as NodeHealth, tcpLatencyMs: payload.tcpLatencyMs, probedAt: payload.probedAt };
  }
  return null;
}

export class ApiClient {
  private csrfToken = "";

  setCsrfToken(token: string): void {
    this.csrfToken = token;
  }

  clearSession(): void {
    this.csrfToken = "";
  }

  async accessStatus(): Promise<AccessStatusResponse> {
    const status = await this.request<AccessStatusResponse>("/access/status");
    if (status.authenticated) this.setCsrfToken(status.csrfToken || "");
    else this.clearSession();
    return status;
  }

  async setupAccess(token: string): Promise<AccessSessionResponse> {
    const session = await this.request<AccessSessionResponse>("/access/setup", { method: "POST", body: { token }, useCSRF: false });
    this.setCsrfToken(session.csrfToken || "");
    return session;
  }

  async loginAccess(token: string): Promise<AccessSessionResponse> {
    const session = await this.request<AccessSessionResponse>("/access/login", { method: "POST", body: { token }, useCSRF: false });
    this.setCsrfToken(session.csrfToken || "");
    return session;
  }
  async status(): Promise<StatusResponse> {
    return this.request<StatusResponse>("/status");
  }

  async nodes(): Promise<NodesResponse> {
    const response = await this.request<NodesResponse>("/nodes");
    return {
      nodes: response.nodes.map((node) => ({
        id: node.id,
        name: node.name,
        group: node.group,
        provider: node.provider,
        health: node.health,
        tcpLatencyMs: node.tcpLatencyMs,
        udpHealth: node.udpHealth,
        eligible: node.eligible,
      })),
    };
  }

  async nodeDetails(id: string): Promise<NodeDetails> {
    return this.request<NodeDetails>(`/nodes/${encodeURIComponent(id)}/details`);
  }



  async login(provider: string, account: string, password: string): Promise<LoginResponse> {
    return this.request<LoginResponse>("/auth/login", {
      method: "POST",
      body: { provider, account, password },
    });
  }

  async logoutAccount(provider: string): Promise<void> {
    await this.request<void>("/auth/logout", { method: "POST", body: { provider }, expectEmptyResponse: true });
  }

  async lockConsole(): Promise<void> {
    try {
      await this.request<void>("/access/logout", { method: "POST", body: {}, expectEmptyResponse: true });
    } catch (error) {
      if (error instanceof ApiError) {
        this.clearSession();
        if (error.problem.status === 401) return;
      }
      throw error;
    }
    this.clearSession();
  }

  async refresh(provider: string): Promise<void> {
    await this.request<void>("/control/refresh", { method: "POST", body: { provider }, expectEmptyResponse: true });
  }

  async probeNode(id: string): Promise<ProbeResult> {
    return this.request<ProbeResult>(`/nodes/${encodeURIComponent(id)}/probe`, {
      method: "POST",
      body: {},
    });
  }


  async smartProxy(): Promise<SmartProxyStatus> { return this.request<SmartProxyStatus>("/smart-proxy"); }

  async configureSmartProxy(enabled: boolean, intervalMinutes: number): Promise<SmartProxyStatus> {
    return this.request<SmartProxyStatus>("/smart-proxy/config", { method: "PUT", body: { enabled, intervalMinutes } });
  }

  async probeSmartProxy(): Promise<void> {
    return this.request<void>("/smart-proxy/probe", { method: "POST", body: {}, expectEmptyResponse: true });
  }

  async smartProxyDetails(): Promise<SmartProxyDetails> { return this.request<SmartProxyDetails>("/smart-proxy/details"); }

  async subscriptionURL(): Promise<SubscriptionURLResponse> {
    return this.request<SubscriptionURLResponse>("/subscription/url");
  }


  /** Downloads the redacted diagnostics report as a JSON blob. */
  async diagnostics(): Promise<Blob> {
    const response = await fetch(`${API_ROOT}/diagnostics/export`, {
      method: "POST",
      credentials: "same-origin",
      cache: "no-store",
      headers: { Accept: "application/json", "Content-Type": "application/json", "X-CSRF-Token": this.csrfToken },
      body: "{}",
    });
    if (!response.ok) throw await asProblem(response);
    return response.blob();
  }

  events(onEvent: (event: EventMessage) => void, onFailure: () => void, onOpen?: () => void): () => void {
    // EventSource reconnects by itself after a dropped connection, but a non-200
    // response (403, 429, 503) closes it permanently. Recreate closed streams
    // with capped exponential backoff so live updates always recover.
    const names = ["state", "refresh", "probe"] as const;
    let stream: EventSource | null = null;
    let retryTimer: ReturnType<typeof setTimeout> | null = null;
    let retryDelay = EVENT_RETRY_INITIAL_MS;
    let stopped = false;
    const listeners = names.map((type) => {
      const listener = (message: Event) => {
        if (stopped) return;
        const data = (message as MessageEvent<unknown>).data;
        if (typeof data !== "string") return;
        const event = parseNamedEvent(type, data);
        if (event) onEvent(event);
      };
      return [type, listener] as const;
    });
    const detach = (current: EventSource) => {
      for (const [type, listener] of listeners) current.removeEventListener(type, listener);
      current.onopen = null;
      current.onerror = null;
      current.close();
    };
    const connect = () => {
      retryTimer = null;
      if (stopped) return;
      const current = new EventSource(`${API_ROOT}/events`, { withCredentials: true });
      let connected = false;
      stream = current;
      for (const [type, listener] of listeners) current.addEventListener(type, listener);
      current.onopen = () => {
        if (stopped || stream !== current || connected) return;
        connected = true;
        retryDelay = EVENT_RETRY_INITIAL_MS;
        // The server does not replay missed events. Reconcile after both the
        // first connection and browser-managed or explicitly recreated streams.
        onOpen?.();
      };
      current.onerror = () => {
        if (stopped || stream !== current) return;
        connected = false;
        onFailure();
        if (current.readyState !== EVENT_SOURCE_CLOSED) return;
        detach(current);
        stream = null;
        retryTimer = setTimeout(connect, retryDelay);
        retryDelay = Math.min(retryDelay * 2, EVENT_RETRY_MAX_MS);
      };
    };
    connect();
    return () => {
      stopped = true;
      if (retryTimer !== null) clearTimeout(retryTimer);
      if (stream) detach(stream);
      stream = null;
    };
  }

  private async request<T>(
    path: string,
    init: { method?: "POST" | "PUT" | "DELETE"; body?: unknown; useCSRF?: boolean; expectEmptyResponse?: boolean } = {},
  ): Promise<T> {
    const isWrite = Boolean(init.method);
    const response = await fetch(`${API_ROOT}${path}`, {
      method: init.method || "GET",
      credentials: "same-origin",
      cache: "no-store",
      headers: {
        Accept: "application/json",
        ...(isWrite
          ? {
              "Content-Type": "application/json",
              ...(init.useCSRF === false ? {} : { "X-CSRF-Token": this.csrfToken }),
            }
          : {}),
      },
      ...(isWrite ? { body: JSON.stringify(init.body ?? {}) } : {}),
    });
    if (!response.ok) throw await asProblem(response);
    if (init.expectEmptyResponse || response.status === 204) return undefined as T;
    return (await response.json()) as T;
  }
}

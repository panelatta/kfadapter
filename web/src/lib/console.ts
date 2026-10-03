import type { NodeRecord, ProbeResult, ServiceState } from "@/types";

export const providerLoginStates: Partial<Record<ServiceState, true>> = {
    signed_out: true,
    expired: true,
};

const providerLabels: Record<string, string> = {
    kuaifan: "KuaiFan",
    quickfox: "QuickFox",
};

export function providerLabel(provider: string): string {
    return providerLabels[provider] ?? provider;
}

export interface StatusDetail {
    label: string;
    message: string;
    tone: "ready" | "working" | "warning" | "danger" | "neutral";
}

export const stateDetails: Record<ServiceState, StatusDetail> = {
    signed_out: {
        label: "Signed out",
        message: "Connect an account to load nodes.",
        tone: "neutral",
    },
    authenticating: {
        label: "Authenticating",
        message: "Connecting your account.",
        tone: "working",
    },
    syncing: { label: "Syncing", message: "Updating nodes.", tone: "working" },
    ready: { label: "Ready", message: "Service is ready.", tone: "ready" },
    degraded: {
        label: "Needs attention",
        message: "Saved nodes are still available.",
        tone: "warning",
    },
    expired: {
        label: "Expired",
        message: "Connect your account again.",
        tone: "danger",
    },
    error: {
        label: "Service error",
        message: "Service status could not be completed.",
        tone: "danger",
    },
};

export const groupNameCollator = new Intl.Collator(["zh-CN-u-co-pinyin", "en"], {
    numeric: true,
    sensitivity: "base",
});
const namedGroupPriorities: Record<string, number> = {
    优选直连线路: 1,
    "音乐/视频APP专线": 2,
};

function groupPriority(group: string): number {
    const namedPriority = namedGroupPriorities[group];
    if (namedPriority !== undefined) return namedPriority;
    return /➩\s*中国(?:大陆)?$/u.test(group) ? 3 : 0;
}

export function compareGroupNames(left: string, right: string): number {
    return (
        groupPriority(left) - groupPriority(right) ||
        groupNameCollator.compare(left, right)
    );
}

/** Applies one successful probe result to the matching node. */
export function applyProbeResult(
    nodes: NodeRecord[],
    result: Pick<ProbeResult, "nodeId" | "health" | "tcpLatencyMs">,
): NodeRecord[] {
    return nodes.map((node) =>
        node.id === result.nodeId
            ? {
                  ...node,
                  health: result.health,
                  tcpLatencyMs: result.tcpLatencyMs,
                  probeError: undefined,
              }
            : node,
    );
}

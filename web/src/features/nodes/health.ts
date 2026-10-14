import { ApiError } from "@/api";
import type { NodeHealth, NodeRecord, ProbeResult } from "@/types";

export function formatLatency(value?: number): string {
    return typeof value === "number" ? `${value} ms` : "—";
}

export function probeFailureLabel(error: unknown): string {
    if (!(error instanceof ApiError)) return "Failed";
    switch (error.problem.code) {
        case "node_ineligible":
            return "Ineligible";
        case "node_not_found":
        case "node_snapshot_unavailable":
            return "Unavailable";
        case "probe_busy":
            return "Busy";
        case "stale_probe":
            return "Changed";
        case "tcp_probe_timeout":
            return "Timeout";
        case "tcp_probe_failed":
            return "Unreachable";
        default:
            return "Failed";
    }
}

/**
 * Only an upstream TCP failure says something about the node itself. Other
 * failures (a busy or stale probe, an ineligible node, a console request
 * error) leave the last known health and latency in place.
 */
function isUpstreamProbeFailure(error: unknown): boolean {
    return (
        error instanceof ApiError &&
        (error.problem.code === "tcp_probe_failed" ||
            error.problem.code === "tcp_probe_timeout")
    );
}

/**
 * Applies one probe outcome to the matching node: a result from a probe
 * response or a probe event, or the error a probe request failed with.
 */
export function applyProbeResult(
    nodes: NodeRecord[],
    outcome:
        | Pick<ProbeResult, "nodeId" | "health" | "tcpLatencyMs">
        | { nodeId: string; error: unknown },
): NodeRecord[] {
    return nodes.map((node) => {
        if (node.id !== outcome.nodeId) return node;
        if (!("error" in outcome))
            return {
                ...node,
                health: outcome.health,
                tcpLatencyMs: outcome.tcpLatencyMs,
                probeError: undefined,
            };
        const unreachable = isUpstreamProbeFailure(outcome.error);
        return {
            ...node,
            health: unreachable ? "unhealthy" : node.health,
            tcpLatencyMs: unreachable ? undefined : node.tcpLatencyMs,
            probeError: probeFailureLabel(outcome.error),
        };
    });
}

export function healthLabel(health: NodeHealth): string {
    return health.slice(0, 1).toUpperCase() + health.slice(1);
}

import { ApiError } from "@/api";
import type { NodeHealth, NodeRecord, ProbeResult } from "@/types";
import { messages } from "@/i18n";
import type { Messages } from "@/i18n";

export function formatLatency(value?: number): string {
    return typeof value === "number" ? `${value} ms` : "—";
}

export type ProbeFailure = keyof Messages["nodes"]["probeFailures"];

export function probeFailure(error: unknown): ProbeFailure {
    if (!(error instanceof ApiError)) return "failed";
    switch (error.problem.code) {
        case "node_ineligible":
            return "ineligible";
        case "node_not_found":
        case "node_snapshot_unavailable":
            return "unavailable";
        case "probe_busy":
            return "busy";
        case "stale_probe":
            return "changed";
        case "tcp_probe_timeout":
            return "timeout";
        case "tcp_probe_failed":
            return "unreachable";
        default:
            return "failed";
    }
}

export function probeFailureLabel(failure: ProbeFailure): string {
    return messages().nodes.probeFailures[failure];
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
            probeError: probeFailure(outcome.error),
        };
    });
}

export function healthLabel(health: NodeHealth): string {
    return messages().nodes.healthLabels[health];
}

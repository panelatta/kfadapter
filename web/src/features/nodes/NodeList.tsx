import { useEffect, useMemo, useRef, useState } from "react";
import type { Dispatch, SetStateAction } from "react";
import { ChevronRight, Gauge } from "lucide-react";
import type { ApiClient } from "@/api";
import type { NodeHealth, NodeRecord } from "@/types";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardAction, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { isSessionProblem } from "@/lib/errors";
import { compareGroupNames, groupNameCollator } from "@/lib/groups";
import { useI18n } from "@/i18n";
import { applyProbeResult, formatLatency, healthLabel, probeFailureLabel } from "./health";
import { NodeDetailsPopover } from "./NodeDetailsPopover";
import { protocolLabel } from "./protocols";

// Batch scope for the header button; group batches use the group name.
const allNodesScope = "\u0000all";

export function NodeList({
    api,
    getSessionRevision,
    nodes,
    onAccessLost,
    onNodesChange,
}: {
    api: ApiClient;
    getSessionRevision: () => number;
    nodes: NodeRecord[];
    onAccessLost: () => void;
    onNodesChange: Dispatch<SetStateAction<NodeRecord[]>>;
}) {
    const { m } = useI18n();
    const [activeProbe, setActiveProbe] = useState<string | null>(null);
    const [batchScope, setBatchScope] = useState<string | null>(null);
    const probing = batchScope !== null || activeProbe !== null;
    const active = useRef(true);

    useEffect(() => {
        active.current = true;
        return () => {
            active.current = false;
        };
    }, []);

    const markProbeFailed = (nodeId: string, requestError: unknown) => {
        onNodesChange((previous) =>
            applyProbeResult(previous, { nodeId, error: requestError }),
        );
    };

    const probe = async (node: NodeRecord) => {
        const revision = getSessionRevision();
        setActiveProbe(node.id);
        try {
            const result = await api.probeNode(node.id);
            if (!active.current || revision !== getSessionRevision()) return;
            onNodesChange((previous) =>
                applyProbeResult(previous, { ...result, nodeId: node.id }),
            );
        } catch (requestError) {
            if (!active.current || revision !== getSessionRevision()) return;
            if (isSessionProblem(requestError)) {
                onAccessLost();
                return;
            }
            markProbeFailed(node.id, requestError);
        } finally {
            if (active.current && revision === getSessionRevision())
                setActiveProbe(null);
        }
    };

    const groupedNodes = useMemo(() => {
        const groups = new Map<string, NodeRecord[]>();
        for (const node of nodes) {
            const group = node.group.trim() || m.nodes.ungrouped;
            const members = groups.get(group);
            if (members) members.push(node);
            else groups.set(group, [node]);
        }
        return [...groups.entries()]
            .sort(([left], [right]) => compareGroupNames(left, right))
            .map(
                ([group, members]) =>
                    [
                        group,
                        members.sort((left, right) =>
                            groupNameCollator.compare(left.name, right.name),
                        ),
                    ] as const,
            );
    }, [nodes, m]);

    const probeBatch = async (targets: readonly NodeRecord[], scope: string) => {
        if (targets.length === 0) return;
        const revision = getSessionRevision();
        setBatchScope(scope);
        try {
            for (const node of targets) {
                if (!active.current || revision !== getSessionRevision())
                    return;
                try {
                    const result = await api.probeNode(node.id);
                    if (!active.current || revision !== getSessionRevision())
                        return;
                    onNodesChange((previous) =>
                        applyProbeResult(previous, {
                            ...result,
                            nodeId: node.id,
                        }),
                    );
                } catch (requestError) {
                    if (!active.current || revision !== getSessionRevision())
                        return;
                    if (isSessionProblem(requestError)) {
                        onAccessLost();
                        return;
                    }
                    markProbeFailed(node.id, requestError);
                }
            }
        } finally {
            if (active.current && revision === getSessionRevision())
                setBatchScope(null);
        }
    };

    return (
        <Card className="min-w-0">
            <CardHeader className="pr-16">
                <CardTitle>{m.nodes.title}</CardTitle>
                <CardDescription>{m.nodes.available(nodes.length)}</CardDescription>
                <CardAction>
                    <Tooltip>
                        <TooltipTrigger asChild>
                            <Button
                                aria-label={m.nodes.checkAll}
                                disabled={probing || nodes.length === 0}
                                onClick={() =>
                                    void probeBatch(
                                        groupedNodes.flatMap(
                                            ([, members]) => members,
                                        ),
                                        allNodesScope,
                                    )
                                }
                                size="sm"
                                variant="outline"
                            >
                                <Gauge
                                    className={
                                        batchScope === allNodesScope
                                            ? "size-4 animate-pulse"
                                            : "size-4"
                                    }
                                />
                                <span aria-hidden="true" className="hidden sm:inline">
                                    {m.nodes.check}
                                </span>
                            </Button>
                        </TooltipTrigger>
                        <TooltipContent>{m.nodes.check}</TooltipContent>
                    </Tooltip>
                </CardAction>
            </CardHeader>
            <CardContent className="min-w-0">
                {/* Native lists and disclosure widgets: each group is a named
                    <details> (role "group") that keyboard and screen-reader
                    users can toggle without a custom tree implementation. */}
                <ul aria-label={m.nodes.byGroup} className="grid gap-2">
                    {groupedNodes.map(([group, members]) => (
                        <li key={group}>
                            <details
                                aria-label={m.nodes.groupLabel(group)}
                                className="node-group overflow-hidden rounded-lg border border-border"
                            >
                                <summary className="flex cursor-pointer list-none items-center gap-2.5 py-1.5 pl-3 pr-1.5 text-sm font-medium transition-colors hover:bg-muted/70 [&::-webkit-details-marker]:hidden">
                                    <ChevronRight
                                        aria-hidden="true"
                                        className="node-group__chevron size-4 text-muted-foreground"
                                    />
                                    <span className="min-w-0 flex-1 truncate py-1">
                                        {group}
                                    </span>
                                    <Badge variant="secondary">
                                        {members.length}
                                    </Badge>
                                    <Tooltip>
                                        <TooltipTrigger asChild>
                                            <Button
                                                aria-label={m.nodes.checkGroup(group)}
                                                className="size-8"
                                                disabled={probing}
                                                onClick={(event) => {
                                                    // Keep the summary from toggling the group.
                                                    event.preventDefault();
                                                    event.stopPropagation();
                                                    void probeBatch(members, group);
                                                }}
                                                size="icon"
                                                variant="ghost"
                                            >
                                                <Gauge
                                                    className={
                                                        batchScope === group
                                                            ? "size-4 animate-pulse"
                                                            : "size-4"
                                                    }
                                                />
                                            </Button>
                                        </TooltipTrigger>
                                        <TooltipContent>
                                            {m.nodes.checkGroupHint}
                                        </TooltipContent>
                                    </Tooltip>
                                </summary>
                                <div className="node-group__content">
                                    <ul className="node-group__content-inner divide-y divide-border border-t border-border bg-well">
                                        {members.map((node) => (
                                            <li
                                                aria-label={node.name}
                                                className="grid grid-cols-[minmax(0,1fr)_auto] items-center gap-x-3 gap-y-1 py-2 pl-4 pr-1.5 sm:grid-cols-[minmax(0,1fr)_6rem_auto] sm:pl-9"
                                                key={node.id}
                                            >
                                                <div className="flex min-w-0 items-center gap-2">
                                                    <span
                                                        aria-hidden="true"
                                                        className={`led ${healthLamp[node.health]}`}
                                                    />
                                                    <p className="truncate text-sm">
                                                        {node.name}
                                                    </p>
                                                    <Badge
                                                        className="shrink-0 px-1.5 py-0 text-[0.6875rem]"
                                                        title={m.nodes.protocol(node.protocol)}
                                                        variant="outline"
                                                    >
                                                        {protocolLabel(
                                                            node.protocol,
                                                        )}
                                                    </Badge>
                                                </div>
                                                <LatencyReadout node={node} />
                                                <div className="col-start-2 row-start-1 flex justify-end sm:col-start-3">
                                                    <NodeDetailsPopover
                                                        api={api}
                                                        getSessionRevision={
                                                            getSessionRevision
                                                        }
                                                        node={node}
                                                        onAccessLost={onAccessLost}
                                                    />
                                                    <Tooltip>
                                                        <TooltipTrigger asChild>
                                                            <Button
                                                                aria-label={m.nodes.checkNode(node.name)}
                                                                disabled={probing}
                                                                onClick={() =>
                                                                    void probe(node)
                                                                }
                                                                size="icon"
                                                                variant="ghost"
                                                            >
                                                                <Gauge
                                                                    className={
                                                                        activeProbe ===
                                                                        node.id
                                                                            ? "size-4 animate-pulse"
                                                                            : "size-4"
                                                                    }
                                                                />
                                                            </Button>
                                                        </TooltipTrigger>
                                                        <TooltipContent>
                                                            {m.nodes.check}
                                                        </TooltipContent>
                                                    </Tooltip>
                                                </div>
                                            </li>
                                        ))}
                                    </ul>
                                </div>
                            </details>
                        </li>
                    ))}
                </ul>
                {groupedNodes.length === 0 ? (
                    <p className="py-8 text-center text-sm text-muted-foreground">
                        {m.nodes.none}
                    </p>
                ) : null}
            </CardContent>
        </Card>
    );
}

const healthLamp: Record<NodeHealth, string> = {
    healthy: "led--ok",
    degraded: "led--warn",
    unhealthy: "led--err",
    unknown: "",
};

function LatencyReadout({ node }: { node: NodeRecord }) {
    const { m } = useI18n();
    const text = node.probeError
        ? probeFailureLabel(node.probeError)
        : formatLatency(node.tcpLatencyMs);
    const tone: Record<NodeHealth, string> = {
        healthy: "text-foreground",
        degraded: "text-warning-foreground",
        unhealthy: "text-destructive",
        unknown: "text-muted-foreground",
    };
    return (
        <span
            aria-label={m.nodes.reachability(text, healthLabel(node.health))}
            className={`col-span-2 pl-4 text-[0.8125rem] tabular-nums sm:col-span-1 sm:pl-0 sm:text-right ${tone[node.health]}`}
        >
            {text}
        </span>
    );
}

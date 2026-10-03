import { useEffect, useMemo, useRef, useState, type Dispatch, type SetStateAction } from "react";
import { ChevronRight, Copy, Gauge, Info, LoaderCircle } from "lucide-react";
import { ApiClient } from "@/api";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardAction, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { groupNameCollator, compareGroupNames, applyProbeResult } from "@/lib/console";
import { isSessionProblem, describeError, probeFailureLabel } from "@/lib/errors";
import { formatLatency, healthLabel } from "@/lib/format";
import { useCopyFeedback } from "@/hooks/useCopyFeedback";
import { FormError } from "@/components/AuthCards";
import type { NodeDetails, NodeHealth, NodeRecord } from "@/types";

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
    const [activeProbe, setActiveProbe] = useState<string | null>(null);
    const [probingAll, setProbingAll] = useState(false);
    const active = useRef(true);

    useEffect(
        () => () => {
            active.current = false;
        },
        [],
    );

    const markProbeFailed = (nodeId: string, requestError: unknown) => {
        onNodesChange((previous) =>
            previous.map((current) =>
                current.id === nodeId
                    ? {
                          ...current,
                          health: "unhealthy",
                          tcpLatencyMs: undefined,
                          probeError: probeFailureLabel(requestError),
                      }
                    : current,
            ),
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
            const group = node.group.trim() || "Ungrouped";
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
    }, [nodes]);

    const probeAll = async () => {
        if (nodes.length === 0) return;
        const revision = getSessionRevision();
        setProbingAll(true);
        try {
            for (const [, members] of groupedNodes) {
                for (const node of members) {
                    if (!active.current || revision !== getSessionRevision())
                        return;
                    try {
                        const result = await api.probeNode(node.id);
                        if (
                            !active.current ||
                            revision !== getSessionRevision()
                        )
                            return;
                        onNodesChange((previous) =>
                            applyProbeResult(previous, {
                                ...result,
                                nodeId: node.id,
                            }),
                        );
                    } catch (requestError) {
                        if (
                            !active.current ||
                            revision !== getSessionRevision()
                        )
                            return;
                        if (isSessionProblem(requestError)) {
                            onAccessLost();
                            return;
                        }
                        markProbeFailed(node.id, requestError);
                    }
                }
            }
        } finally {
            if (active.current && revision === getSessionRevision())
                setProbingAll(false);
        }
    };

    return (
        <Card className="panel min-w-0">
            <CardHeader>
                <CardTitle>Nodes</CardTitle>
                <CardDescription>{nodes.length} available</CardDescription>
                <CardAction>
                    <Tooltip>
                        <TooltipTrigger asChild>
                            <Button
                                aria-label="Check TCP reachability for all nodes"
                                disabled={
                                    probingAll ||
                                    activeProbe !== null ||
                                    nodes.length === 0
                                }
                                onClick={() => void probeAll()}
                                size="icon"
                                variant="ghost"
                            >
                                <Gauge
                                    className={
                                        probingAll
                                            ? "size-4 animate-pulse"
                                            : "size-4"
                                    }
                                />
                            </Button>
                        </TooltipTrigger>
                        <TooltipContent>Check TCP reachability</TooltipContent>
                    </Tooltip>
                </CardAction>
            </CardHeader>
            <CardContent className="min-w-0">
                {/* Native lists and disclosure widgets: each group is a named
                    <details> (role "group") that keyboard and screen-reader
                    users can toggle without a custom tree implementation. */}
                <ul aria-label="Nodes by group" className="grid gap-2.5">
                    {groupedNodes.map(([group, members], groupIndex) => (
                        <li key={group}>
                        <details
                            aria-label={`${group} group`}
                            className="node-group overflow-hidden rounded-sm border border-border bg-background/40"
                        >
                            <summary className="flex cursor-pointer list-none items-center gap-2.5 px-3 py-2.5 text-sm font-medium transition-colors hover:bg-primary/5 [&::-webkit-details-marker]:hidden">
                                <ChevronRight
                                    aria-hidden="true"
                                    className="node-group__chevron size-4 text-muted-foreground"
                                />
                                <span
                                    aria-hidden="true"
                                    className="font-sans text-xs font-semibold tabular-nums text-primary/80"
                                >
                                    {String(groupIndex + 1).padStart(2, "0")}
                                </span>
                                <span className="min-w-0 flex-1 truncate">
                                    {group}
                                </span>
                                <Badge variant="secondary">
                                    {members.length}
                                </Badge>
                            </summary>
                            <div className="node-group__content">
                                <ul className="node-group__content-inner divide-y divide-border border-t border-border">
                                    {members.map((node) => (
                                        <li
                                            aria-label={node.name}
                                            className="grid gap-3 p-3 transition-colors hover:bg-primary/[0.04] sm:grid-cols-[minmax(0,1fr)_auto_auto] sm:items-center"
                                            key={node.id}
                                        >
                                            <div className="min-w-0">
                                                <p className="truncate text-sm font-medium">
                                                    {node.name}
                                                </p>
                                                <p className="mt-0.5 text-[0.65rem] uppercase tracking-[0.18em] text-muted-foreground">
                                                    {node.provider}
                                                </p>
                                            </div>
                                            <LatencyReadout node={node} />
                                            <div className="flex justify-end gap-1">
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
                                                            aria-label={`Check TCP reachability for ${node.name}`}
                                                            disabled={
                                                                probingAll ||
                                                                activeProbe !==
                                                                    null
                                                            }
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
                                                        Check TCP reachability
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
                    <p className="px-1 py-8 text-sm text-muted-foreground">
                        No nodes available.
                    </p>
                ) : null}
            </CardContent>
        </Card>
    );
}

function LatencyReadout({ node }: { node: NodeRecord }) {
    const text = node.probeError || formatLatency(node.tcpLatencyMs);
    const tone: Record<NodeHealth, string> = {
        healthy: "text-led-ok",
        degraded: "text-led-warn",
        unhealthy: "text-led-err",
        unknown: "text-muted-foreground",
    };
    return (
        <span
            aria-label={`TCP reachability ${text}, ${healthLabel(node.health)}`}
            className={`text-xs font-medium tabular-nums sm:text-right ${tone[node.health]}`}
        >
            {text}
        </span>
    );
}

function NodeDetailsPopover({
    api,
    getSessionRevision,
    node,
    onAccessLost,
}: {
    api: ApiClient;
    getSessionRevision: () => number;
    node: NodeRecord;
    onAccessLost: () => void;
}) {
    const [open, setOpen] = useState(false);
    const [details, setDetails] = useState<NodeDetails | null>(null);
    const [error, setError] = useState("");
    const [loading, setLoading] = useState(false);
    const requestId = useRef(0);

    useEffect(
        () => () => {
            requestId.current += 1;
        },
        [],
    );

    const onOpenChange = (nextOpen: boolean) => {
        requestId.current += 1;
        const currentRequest = requestId.current;
        setOpen(nextOpen);
        setDetails(null);
        setError("");
        setLoading(nextOpen);
        if (!nextOpen) return;

        const revision = getSessionRevision();
        api.nodeDetails(node.id)
            .then((response) => {
                if (
                    currentRequest !== requestId.current ||
                    revision !== getSessionRevision()
                )
                    return;
                setDetails(response);
            })
            .catch((requestError: unknown) => {
                if (
                    currentRequest !== requestId.current ||
                    revision !== getSessionRevision()
                )
                    return;
                if (isSessionProblem(requestError)) {
                    onAccessLost();
                    return;
                }
                setError(describeError(requestError));
            })
            .finally(() => {
                if (
                    currentRequest === requestId.current &&
                    revision === getSessionRevision()
                )
                    setLoading(false);
            });
    };

    return (
        <Popover onOpenChange={onOpenChange} open={open}>
            <Tooltip>
                <TooltipTrigger asChild>
                    <PopoverTrigger asChild>
                        <Button
                            aria-label={`Details for ${node.name}`}
                            size="icon"
                            variant="ghost"
                        >
                            <Info className="size-4" />
                        </Button>
                    </PopoverTrigger>
                </TooltipTrigger>
                <TooltipContent>Details</TooltipContent>
            </Tooltip>
            <PopoverContent
                align="end"
                className="panel w-[min(25rem,calc(100vw-2rem))] p-0"
            >
                <div className="flex flex-col gap-4 py-4">
                    <div className="grid gap-1 px-4">
                        <h2 className="font-sans text-sm font-semibold uppercase tracking-[0.1em]">
                            {node.name}
                        </h2>
                        <p className="text-xs text-muted-foreground">
                            {node.group} · {node.provider}
                        </p>
                    </div>
                    <div className="px-4">
                        {loading ? (
                            <p className="flex items-center gap-2 text-sm text-muted-foreground">
                                <LoaderCircle className="size-4 animate-spin" />
                                Loading details
                            </p>
                        ) : null}
                        {error ? <FormError message={error} /> : null}
                        {details ? <NodeDetailsView details={details} /> : null}
                    </div>
                </div>
            </PopoverContent>
        </Popover>
    );
}

function NodeDetailsView({ details }: { details: NodeDetails }) {
    const socksURL = `socks5://${encodeURIComponent(details.socksUsername)}:${encodeURIComponent(details.socksPassword)}@${details.socksAddress}`;
    const { message: copyMessage, copy: copySocksURL } = useCopyFeedback(
        socksURL,
        "SOCKS5 URL copied.",
    );

    return (
        <div className="grid gap-4">
            <div className="grid gap-2.5 text-sm">
                <dl>
                    <DetailRow
                        label="Upstream"
                        value={`${details.upstreamHost}:${details.upstreamPort}`}
                    />
                </dl>
                <dl className="grid grid-cols-3 gap-3 border-t border-dashed border-border pt-3">
                    <DetailRow
                        label="Health"
                        value={healthLabel(details.health)}
                    />
                    <DetailRow
                        label="Latency"
                        value={formatLatency(details.tcpLatencyMs)}
                    />
                </dl>
            </div>
            <div className="flex flex-wrap items-center gap-3">
                <Button
                    onClick={() => void copySocksURL()}
                    size="sm"
                    variant="outline"
                >
                    <Copy className="size-4" />
                    Copy SOCKS5 URL
                </Button>
                <p
                    aria-live="polite"
                    className="min-h-5 text-xs text-muted-foreground"
                    role="status"
                >
                    {copyMessage}
                </p>
            </div>
        </div>
    );
}

function DetailRow({ label, value }: { label: string; value: string }) {
    return (
        <div className="min-w-0">
            <dt className="text-[0.625rem] font-semibold uppercase tracking-[0.18em] text-muted-foreground">
                {label}
            </dt>
            <dd className="mt-1 break-all font-mono text-xs text-primary/90">
                {value}
            </dd>
        </div>
    );
}

import { Copy, RefreshCw } from "lucide-react";
import type { ApiClient } from "@/api";
import type { SmartProxyMeasurement, SmartProxyStatus } from "@/types";
import { selectClassName } from "@/components/forms";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { formatTime } from "@/lib/time";
import { useSmartProxy } from "./useSmartProxy";

export function SmartProxyCard({
    api,
    getSessionRevision,
    onAccessLost,
}: {
    api: ApiClient;
    getSessionRevision: () => number;
    onAccessLost: () => void;
}) {
    const {
        busy,
        configure,
        copyAddress,
        error,
        loadError,
        message,
        probe,
        status,
    } = useSmartProxy({ api, getSessionRevision, onAccessLost });
    const alert = error || loadError;

    return (
        <Card className="panel">
            <CardHeader>
                <CardTitle>Smart SOCKS5 proxy</CardTitle>
                <CardDescription>
                    One stable address, automatically routed through the best
                    measured domestic line.
                </CardDescription>
            </CardHeader>
            <CardContent className="space-y-4">
                <div className="flex flex-wrap items-center gap-4">
                    <label className="flex items-center gap-2 text-sm">
                        <input
                            aria-label="Enable smart SOCKS5 proxy"
                            checked={status?.enabled ?? false}
                            disabled={!status || busy}
                            onChange={(event) =>
                                configure(
                                    event.target.checked,
                                    status?.intervalMinutes ?? 30,
                                )
                            }
                            role="switch"
                            type="checkbox"
                        />
                        Enable smart proxy
                    </label>
                    <label className="flex items-center gap-2 text-sm">
                        Probe interval
                        <select
                            aria-label="Smart proxy probe interval"
                            className={selectClassName}
                            disabled={!status || busy}
                            onChange={(event) =>
                                configure(
                                    status?.enabled ?? false,
                                    Number(event.target.value),
                                )
                            }
                            value={status?.intervalMinutes ?? 30}
                        >
                            <option value={30}>30 minutes</option>
                            <option value={60}>60 minutes</option>
                        </select>
                    </label>
                    <Button
                        disabled={!status?.enabled || status.running || busy}
                        onClick={probe}
                        variant="outline"
                    >
                        <RefreshCw
                            className={status?.running ? "animate-spin" : ""}
                        />
                        {status?.running ? "Probing…" : "Probe now"}
                    </Button>
                    <Button
                        disabled={!status?.enabled || busy}
                        onClick={copyAddress}
                        variant="outline"
                    >
                        <Copy />
                        Copy smart SOCKS5 address
                    </Button>
                </div>
                <p aria-live="polite" className="text-sm">
                    {selectionSummary(status, Boolean(loadError))}
                </p>
                <p className="text-xs text-muted-foreground">
                    Tests Bilibili, Xiaohongshu, WeChat and QQ through each
                    proxy. Ranks by successful sites, then average HTTPS
                    response time. This measures website responsiveness, not
                    download bandwidth or in-app call quality. New connections
                    use the selected line; existing connections keep their
                    line.
                </p>
                {status?.enabled ? (
                    <p className="text-xs text-muted-foreground">
                        Last probe: {formatTime(status.lastRunAt)} · Next
                        probe: {formatTime(status.nextRunAt)}
                    </p>
                ) : null}
                {status?.enabled && status.incomplete ? (
                    <p className="text-xs text-muted-foreground">
                        Only {status.results.length} of {status.candidateCount}{" "}
                        lines completed this round. Results can be partial
                        after the 10 minute limit or an account refresh; the
                        next round rotates through the remaining lines.
                    </p>
                ) : null}
                {status && status.results.length > 0 ? (
                    <MeasurementTable status={status} />
                ) : null}
                {alert ? (
                    <p className="text-sm text-destructive" role="alert">
                        {alert}
                    </p>
                ) : null}
                <p className="text-xs text-muted-foreground" role="status">
                    {message}
                </p>
            </CardContent>
        </Card>
    );
}

function selectionSummary(
    status: SmartProxyStatus | null,
    unavailable: boolean,
): string {
    if (!status)
        return unavailable
            ? "Smart proxy status is unavailable. Retrying…"
            : "Loading smart proxy…";
    if (!status.enabled) return "Disabled";
    if (status.selectedName) return `Current line: ${status.selectedName}`;
    return "Waiting for a usable measured line. New connections are unavailable until a line passes.";
}

function measurementLabel(measurement: SmartProxyMeasurement): string {
    if (measurement.ok) return `${measurement.latencyMs} ms`;
    if (measurement.error === "timeout") return "Timed out";
    if (measurement.error === "http_rejected") return "Site rejected request";
    return "Failed";
}

function MeasurementTable({ status }: { status: SmartProxyStatus }) {
    return (
        <details>
            <summary className="cursor-pointer text-sm">
                Line measurements ({status.results.length})
            </summary>
            <div className="mt-3 overflow-x-auto">
                <table className="w-full text-left text-xs">
                    <thead>
                        <tr>
                            <th className="p-2">Line</th>
                            <th className="p-2">Sites reached</th>
                            <th className="p-2">Average</th>
                            {status.targets.map((target) => (
                                <th className="p-2" key={target.name}>
                                    {target.name}
                                </th>
                            ))}
                        </tr>
                    </thead>
                    <tbody>
                        {status.results.map((result) => (
                            <tr
                                className="border-t border-border/60"
                                key={result.nodeId}
                            >
                                <td className="p-2">
                                    {result.nodeId === status.selectedNodeId
                                        ? "✓ "
                                        : ""}
                                    {result.name} ({result.provider})
                                </td>
                                <td className="p-2">
                                    {result.successes}/{status.targets.length}
                                </td>
                                <td className="p-2">
                                    {result.successes
                                        ? `${result.latencyMs} ms`
                                        : "Unavailable"}
                                </td>
                                {result.measurements.map((measurement) => (
                                    <td className="p-2" key={measurement.target}>
                                        {measurementLabel(measurement)}
                                    </td>
                                ))}
                            </tr>
                        ))}
                    </tbody>
                </table>
            </div>
        </details>
    );
}

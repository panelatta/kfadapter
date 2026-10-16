import { ChevronRight, Copy, RefreshCw } from "lucide-react";
import type { ApiClient } from "@/api";
import type { SmartProxyMeasurement, SmartProxyStatus } from "@/types";
import { selectClassName } from "@/components/forms";
import { Button } from "@/components/ui/button";
import { Card, CardAction, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { providerLabel } from "@/features/accounts/providers";
import { messages, useI18n } from "@/i18n";
import { formatTime } from "@/lib/time";
import { cn } from "@/lib/utils";
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
    const { m } = useI18n();
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
    const lineTone = !status?.enabled
        ? ""
        : status.selectedName
          ? "led--ok"
          : "led--warn";

    return (
        <Card>
            <CardHeader className="pr-24">
                <CardTitle>{m.smart.title}</CardTitle>
                <CardDescription>{m.smart.description}</CardDescription>
                <CardAction className="top-0.5">
                    <label className="relative inline-flex cursor-pointer items-center gap-2 text-sm has-disabled:cursor-not-allowed has-disabled:opacity-50">
                        <span className="sr-only sm:not-sr-only sm:text-muted-foreground">
                            {m.smart.enable}
                        </span>
                        <input
                            aria-label={m.smart.enableLabel}
                            checked={status?.enabled ?? false}
                            className="peer sr-only"
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
                        <span
                            aria-hidden="true"
                            className="relative h-6 w-10 shrink-0 rounded-full bg-input transition-colors after:absolute after:left-0.5 after:top-0.5 after:size-5 after:rounded-full after:bg-card after:shadow-sm after:transition-transform peer-checked:bg-primary peer-checked:after:translate-x-4 peer-focus-visible:outline-2 peer-focus-visible:outline-offset-2 peer-focus-visible:outline-ring"
                        />
                    </label>
                </CardAction>
            </CardHeader>
            <CardContent className="grid gap-4">
                <div className="flex items-start gap-2.5 rounded-lg bg-muted px-4 py-3">
                    <span
                        aria-hidden="true"
                        className={cn(
                            "led mt-2",
                            lineTone,
                            status?.running && "led--warn led--pulse",
                        )}
                    />
                    <p aria-live="polite" className="min-w-0 font-medium">
                        {selectionSummary(status, Boolean(loadError))}
                    </p>
                </div>
                <div className="flex flex-wrap items-center gap-2">
                    <label className="mr-auto flex items-center gap-2 text-sm text-muted-foreground">
                        {m.smart.interval}
                        <select
                            aria-label={m.smart.intervalLabel}
                            className={cn(selectClassName, "w-auto text-foreground")}
                            disabled={!status || busy}
                            onChange={(event) =>
                                configure(
                                    status?.enabled ?? false,
                                    Number(event.target.value),
                                )
                            }
                            value={status?.intervalMinutes ?? 30}
                        >
                            <option value={30}>{m.smart.minutes(30)}</option>
                            <option value={60}>{m.smart.minutes(60)}</option>
                        </select>
                    </label>
                    <Button
                        disabled={!status?.enabled || status.running || busy}
                        onClick={probe}
                        size="sm"
                        variant="outline"
                    >
                        <RefreshCw
                            className={status?.running ? "animate-spin" : ""}
                        />
                        {status?.running ? m.smart.probing : m.smart.probe}
                    </Button>
                    <Button
                        disabled={!status?.enabled || busy}
                        onClick={copyAddress}
                        size="sm"
                        variant="outline"
                    >
                        <Copy />
                        {m.smart.copyAddress}
                    </Button>
                </div>
                {status?.enabled ? (
                    <p className="text-[0.8125rem] text-muted-foreground">
                        {m.smart.schedule(
                            formatTime(status.lastRunAt),
                            formatTime(status.nextRunAt),
                        )}
                    </p>
                ) : null}
                {status?.enabled && status.incomplete ? (
                    <p className="text-[0.8125rem] text-muted-foreground">
                        {m.smart.incomplete(
                            status.results.length,
                            status.candidateCount ?? status.results.length,
                        )}
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
                <p
                    className="text-[0.8125rem] text-muted-foreground empty:hidden"
                    role="status"
                >
                    {message}
                </p>
                <div className="border-t border-border pt-4">
                    <p className="max-w-prose text-[0.8125rem] leading-5 text-muted-foreground">
                        {m.smart.method}
                    </p>
                </div>
            </CardContent>
        </Card>
    );
}

function selectionSummary(
    status: SmartProxyStatus | null,
    unavailable: boolean,
): string {
    const m = messages().smart;
    if (!status) return unavailable ? m.unavailable : m.loading;
    if (!status.enabled) return m.disabled;
    if (status.selectedName) return m.currentLine(status.selectedName);
    return m.waiting;
}

function measurementLabel(measurement: SmartProxyMeasurement): string {
    const m = messages().smart;
    if (measurement.ok) return `${measurement.latencyMs} ms`;
    if (measurement.error === "timeout") return m.timedOut;
    if (measurement.error === "http_rejected") return m.rejected;
    return m.failed;
}

function MeasurementTable({ status }: { status: SmartProxyStatus }) {
    const { m } = useI18n();
    return (
        <details className="node-group group rounded-lg border border-border">
            <summary className="flex cursor-pointer list-none items-center gap-2 px-3 py-2.5 text-sm font-medium [&::-webkit-details-marker]:hidden">
                <ChevronRight
                    aria-hidden="true"
                    className="node-group__chevron size-4 text-muted-foreground"
                />
                {m.smart.measurements(status.results.length)}
            </summary>
            <div className="overflow-x-auto border-t border-border">
                <table className="w-full text-left text-[0.8125rem] tabular-nums">
                    <thead className="bg-muted text-muted-foreground">
                        <tr>
                            <th className="px-3 py-2 font-medium">{m.smart.line}</th>
                            <th className="px-3 py-2 font-medium">
                                {m.smart.sitesReached}
                            </th>
                            <th className="px-3 py-2 font-medium">{m.smart.average}</th>
                            {status.targets.map((target) => (
                                <th className="px-3 py-2 font-medium" key={target.name}>
                                    {target.name}
                                </th>
                            ))}
                        </tr>
                    </thead>
                    <tbody>
                        {status.results.map((result) => {
                            const selected =
                                result.nodeId === status.selectedNodeId;
                            return (
                                <tr
                                    className={cn(
                                        "border-t border-border",
                                        selected && "bg-success/60",
                                    )}
                                    key={result.nodeId}
                                >
                                    <td className="px-3 py-2">
                                        {selected ? "✓ " : ""}
                                        {result.name}{" "}
                                        <span className="text-muted-foreground">
                                            ({providerLabel(result.provider)})
                                        </span>
                                    </td>
                                    <td className="px-3 py-2">
                                        {result.successes}/{status.targets.length}
                                    </td>
                                    <td className="px-3 py-2">
                                        {result.successes
                                            ? `${result.latencyMs} ms`
                                            : m.smart.averageUnavailable}
                                    </td>
                                    {result.measurements.map((measurement) => (
                                        <td
                                            className="px-3 py-2"
                                            key={measurement.target}
                                        >
                                            {measurementLabel(measurement)}
                                        </td>
                                    ))}
                                </tr>
                            );
                        })}
                    </tbody>
                </table>
            </div>
        </details>
    );
}

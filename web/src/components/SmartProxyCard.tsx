import { useEffect, useRef, useState } from "react";
import { Copy, RefreshCw } from "lucide-react";
import { ApiClient } from "@/api";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { selectClassName } from "@/components/AuthCards";
import { copyText } from "@/lib/clipboard";
import { describeError, isSessionProblem } from "@/lib/errors";
import { formatTime } from "@/lib/format";
import type { SmartProxyStatus } from "@/types";

export function SmartProxyCard({ api, getSessionRevision, onAccessLost }: {
    api: ApiClient;
    getSessionRevision: () => number;
    onAccessLost: () => void;
}) {
    const [status, setStatus] = useState<SmartProxyStatus | null>(null);
    const [error, setError] = useState("");
    const [loadError, setLoadError] = useState("");
    const [message, setMessage] = useState("");
    const [busy, setBusy] = useState(false);
    const operation = useRef(0);
    const operationPending = useRef(false);
    const mounted = useRef(false);
    const sessionRevision = useRef<number | null>(null);

    useEffect(() => {
        mounted.current = true;
        let active = true;
        let timer: ReturnType<typeof setTimeout>;
        const revision = getSessionRevision();
        sessionRevision.current = revision;
        const isCurrentSession = () => active && mounted.current && revision === getSessionRevision();
        const load = async () => {
            if (!isCurrentSession()) return;
            const sequence = operation.current;
            const isCurrent = () => isCurrentSession() && sequence === operation.current;
            try {
                // A poll started during a write must not publish its older
                // snapshot or revoke access while the write is completing.
                if (operationPending.current) return;
                const next = await api.smartProxy();
                if (isCurrent()) {
                    setStatus(next);
                    setLoadError("");
                    setBusy(false);
                }
            } catch (cause) {
                if (isCurrent()) {
                    if (isSessionProblem(cause)) {
                        mounted.current = false;
                        onAccessLost();
                    } else setLoadError(describeError(cause));
                }
            } finally {
                if (isCurrentSession()) timer = setTimeout(() => { void load(); }, 5000);
            }
        };
        void load();
        return () => {
            active = false;
            mounted.current = false;
            operation.current += 1;
            operationPending.current = false;
            clearTimeout(timer);
        };
    }, [api, getSessionRevision, onAccessLost]);

    const run = async (work: (isCurrent: () => boolean) => Promise<void>) => {
        const revision = getSessionRevision();
        if (!mounted.current || operationPending.current || revision !== sessionRevision.current) return;
        const sequence = ++operation.current;
        operationPending.current = true;
        const isCurrent = () => mounted.current && revision === getSessionRevision() && sequence === operation.current;
        setBusy(true);
        setError("");
        setMessage("");
        try { await work(isCurrent); }
        catch (cause) {
            if (isCurrent()) {
                if (isSessionProblem(cause)) {
                    mounted.current = false;
                    onAccessLost();
                }
                else setError(describeError(cause));
            }
        } finally {
            if (sequence === operation.current) {
                if (isCurrent()) setBusy(false);
                operation.current += 1;
                operationPending.current = false;
            }
        }
    };
    const configure = (enabled: boolean, intervalMinutes: number) => {
        void run(async (isCurrent) => {
            const next = await api.configureSmartProxy(enabled, intervalMinutes);
            if (isCurrent()) {
                setStatus(next);
                setLoadError("");
            }
        });
    };
    const copy = () => {
        void run(async (isCurrent) => {
            const details = await api.smartProxyDetails();
            if (!isCurrent()) return;
            const copied = await copyText(details.url, isCurrent);
            if (isCurrent()) setMessage(copied ? "Smart SOCKS5 address copied." : "Could not copy the address. Check clipboard permissions.");
        });
    };
    return (
        <Card className="panel">
            <CardHeader>
                <CardTitle>Smart SOCKS5 proxy</CardTitle>
                <CardDescription>One stable address, automatically routed through the best measured domestic line.</CardDescription>
            </CardHeader>
            <CardContent className="space-y-4">
                <div className="flex flex-wrap items-center gap-4">
                    <label className="flex items-center gap-2 text-sm">
                        <input type="checkbox" role="switch" aria-label="Enable smart SOCKS5 proxy" checked={status?.enabled ?? false} disabled={!status || busy} onChange={(event) => configure(event.target.checked, status?.intervalMinutes ?? 30)} />
                        Enable smart proxy
                    </label>
                    <label className="flex items-center gap-2 text-sm">
                        Probe interval
                        <select aria-label="Smart proxy probe interval" className={selectClassName} value={status?.intervalMinutes ?? 30} disabled={!status || busy} onChange={(event) => configure(status?.enabled ?? false, Number(event.target.value))}>
                            <option value={30}>30 minutes</option><option value={60}>60 minutes</option>
                        </select>
                    </label>
                    <Button variant="outline" disabled={!status?.enabled || status.running || busy} onClick={() => { void run(async (isCurrent) => { await api.probeSmartProxy(); if (isCurrent()) { setStatus((previous) => previous ? { ...previous, running: true } : previous); setMessage("Probe requested."); } }); }}>
                        <RefreshCw className={status?.running ? "animate-spin" : ""} />{status?.running ? "Probing…" : "Probe now"}
                    </Button>
                    <Button variant="outline" disabled={!status?.enabled || busy} onClick={copy}><Copy />Copy smart SOCKS5 address</Button>
                </div>
                <p className="text-sm" aria-live="polite">{!status ? loadError ? "Smart proxy status is unavailable. Retrying…" : "Loading smart proxy…" : !status.enabled ? "Disabled" : status.selectedName ? `Current line: ${status.selectedName}` : "Waiting for a usable measured line. New connections are unavailable until a line passes."}</p>
                <p className="text-xs text-muted-foreground">Tests Bilibili, Xiaohongshu, WeChat and QQ through each proxy. Ranks by successful sites, then average HTTPS response time. This measures website responsiveness, not download bandwidth or in-app call quality. New connections use the selected line; existing connections keep their line.</p>
                {status?.enabled && <p className="text-xs text-muted-foreground">Last probe: {formatTime(status.lastRunAt)} · Next probe: {formatTime(status.nextRunAt)}</p>}
                {status?.enabled && status.incomplete && <p className="text-xs text-muted-foreground">Only {status.results.length} of {status.candidateCount} lines completed this round. Results can be partial after the 10 minute limit or an account refresh; the next round rotates through the remaining lines.</p>}
                {Boolean(status?.results.length) && <details>
                    <summary className="cursor-pointer text-sm">Line measurements ({status?.results.length})</summary>
                    <div className="mt-3 overflow-x-auto">
                        <table className="w-full text-left text-xs">
                            <thead><tr><th className="p-2">Line</th><th className="p-2">Sites reached</th><th className="p-2">Average</th>{status?.targets.map((target) => <th className="p-2" key={target.name}>{target.name}</th>)}</tr></thead>
                            <tbody>{status?.results.map((result) => <tr key={result.nodeId} className="border-t border-border/60">
                                <td className="p-2">{result.nodeId === status.selectedNodeId ? "✓ " : ""}{result.name} ({result.provider})</td><td className="p-2">{result.successes}/{status.targets.length}</td><td className="p-2">{result.successes ? `${result.latencyMs} ms` : "Unavailable"}</td>
                                {result.measurements.map((measurement) => <td className="p-2" key={measurement.target}>{measurement.ok ? `${measurement.latencyMs} ms` : measurement.error === "timeout" ? "Timed out" : measurement.error === "http_rejected" ? "Site rejected request" : "Failed"}</td>)}
                            </tr>)}</tbody>
                        </table>
                    </div>
                </details>}
                {(error || loadError) && <p role="alert" className="text-sm text-destructive">{error || loadError}</p>}
                <p role="status" className="text-xs text-muted-foreground">{message}</p>
            </CardContent>
        </Card>
    );
}

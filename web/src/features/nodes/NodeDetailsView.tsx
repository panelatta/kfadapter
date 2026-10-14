import { Copy } from "lucide-react";
import type { NodeDetails } from "@/types";
import { Button } from "@/components/ui/button";
import { useCopyFeedback } from "@/hooks/useCopyFeedback";
import { formatLatency, healthLabel } from "./health";

export function NodeDetailsView({ details }: { details: NodeDetails }) {
    const socksURL = `socks5://${encodeURIComponent(details.socksUsername)}:${encodeURIComponent(details.socksPassword)}@${details.socksAddress}`;
    const { message: copyMessage, copy: copySocksURL } = useCopyFeedback(
        socksURL,
        "SOCKS5 URL copied.",
    );

    return (
        <div className="grid gap-4">
            {/* Each <dl> holds only name/value groups; layout wrappers stay
                outside the description lists. */}
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

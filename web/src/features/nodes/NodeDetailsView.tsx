import { Copy } from "lucide-react";
import type { NodeDetails } from "@/types";
import { Button } from "@/components/ui/button";
import { useCopyStatus } from "@/hooks/useCopyFeedback";
import { useI18n } from "@/i18n";
import { formatLatency, healthLabel } from "./health";

export function NodeDetailsView({ details }: { details: NodeDetails }) {
    const { m } = useI18n();
    const socksURL = `socks5://${encodeURIComponent(details.socksUsername)}:${encodeURIComponent(details.socksPassword)}@${details.socksAddress}`;
    const { message: copyMessage, copy } = useCopyStatus();
    const profiles = details.profiles ?? [];
    const shadowsocks = profiles.find((profile) => profile.protocol === "shadowsocks" && profile.uri);

    return (
        <div className="grid gap-4">
            {/* Each <dl> holds only name/value groups; layout wrappers stay
                outside the description lists. */}
            <div className="grid gap-2.5 text-sm">
                <dl>
                    <DetailRow
                        label={m.nodes.upstream}
                        value={`${details.upstreamHost}:${details.upstreamPort}`}
                    />
                </dl>
                <dl className="grid grid-cols-2 gap-3 border-t border-border pt-3">
                    <DetailRow
                        label={m.nodes.health}
                        value={healthLabel(details.health)}
                    />
                    <DetailRow
                        label={m.nodes.latency}
                        value={formatLatency(details.tcpLatencyMs)}
                    />
                </dl>
            </div>
            <div className="flex flex-wrap items-center gap-3">
                <Button
                    onClick={() => void copy(socksURL, m.nodes.socksCopied)}
                    size="sm"
                    variant="outline"
                >
                    <Copy className="size-4" />
                    {m.nodes.copySocks}
                </Button>
                {shadowsocks?.uri ? (
                    <Button
                        aria-label={m.nodes.copyShadowsocksLabel}
                        onClick={() => void copy(shadowsocks.uri ?? "", m.nodes.shadowsocksCopied)}
                        size="sm"
                        variant="outline"
                    >
                        <Copy className="size-4" />
                        {m.nodes.copyShadowsocks}
                    </Button>
                ) : null}
            </div>
            {profiles.length > 0 ? (
                <div className="grid gap-2 border-t border-border pt-3">
                    <p className="text-[0.8125rem] text-muted-foreground">{m.nodes.clientLines}</p>
                    <div className="flex flex-wrap gap-2">
                        {profiles.map((profile) => {
                            const name = m.inbounds.names[profile.protocol] ?? profile.protocol;
                            return (
                                <Button
                                    aria-label={m.nodes.copySurgeLabel(name)}
                                    key={profile.protocol}
                                    onClick={() => void copy(profile.surge, m.nodes.surgeCopied)}
                                    size="sm"
                                    variant="outline"
                                >
                                    <Copy className="size-4" />
                                    {m.nodes.copySurge(name)}
                                </Button>
                            );
                        })}
                    </div>
                </div>
            ) : null}
            <p
                aria-live="polite"
                className="min-h-5 text-[0.8125rem] text-muted-foreground"
                role="status"
            >
                {copyMessage}
            </p>
        </div>
    );
}

function DetailRow({ label, value }: { label: string; value: string }) {
    return (
        <div className="min-w-0">
            <dt className="text-[0.8125rem] text-muted-foreground">
                {label}
            </dt>
            <dd className="mt-0.5 break-all font-mono text-[0.8125rem] text-foreground">
                {value}
            </dd>
        </div>
    );
}

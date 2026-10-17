import type { InboundChoices, InboundStatus } from "@/types";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { useI18n } from "@/i18n";
import { cn } from "@/lib/utils";
import { choicesFrom, type useInbounds } from "./useInbounds";

const choiceKeys: Record<string, keyof InboundChoices> = {
    socks5: "socks5",
    snell: "snell",
    shadowsocks: "shadowsocks",
    "socks5-tls": "socksTLS",
};

export function InboundsCard({ inbounds }: { inbounds: ReturnType<typeof useInbounds> }) {
    const { m } = useI18n();
    const { busy, configure, error, loadError, status } = inbounds;
    const alert = error || loadError;
    const choices = choicesFrom(status);

    return (
        <Card>
            <CardHeader>
                <CardTitle>{m.inbounds.title}</CardTitle>
                <CardDescription>{m.inbounds.description}</CardDescription>
            </CardHeader>
            <CardContent className="grid gap-4">
                {status ? (
                    <ul aria-label={m.inbounds.title} className="grid gap-2">
                        {status.inbounds.map((inbound) => {
                            const key = choiceKeys[inbound.protocol];
                            const name = m.inbounds.names[inbound.protocol] ?? inbound.protocol;
                            return (
                                <li
                                    className="flex items-center gap-3 rounded-lg border border-border px-3.5 py-3"
                                    key={inbound.protocol}
                                >
                                    <span aria-hidden="true" className={cn("led shrink-0", ledTone(inbound))} />
                                    <div className="min-w-0 flex-1">
                                        <p className="flex flex-wrap items-baseline gap-x-2 font-medium">
                                            {name}
                                            <span className="font-mono text-[0.8125rem] font-normal text-muted-foreground">
                                                {m.inbounds.port(inbound.port)}
                                            </span>
                                        </p>
                                        <p className="text-[0.8125rem] text-muted-foreground">
                                            {m.inbounds.details[inbound.protocol] ?? ""}
                                        </p>
                                        {inbound.protocol === "socks5-tls" && inbound.enabled && status.tlsFingerprint ? (
                                            <p className="mt-1 break-all font-mono text-xs text-muted-foreground">
                                                {m.inbounds.fingerprint}: {status.tlsFingerprint}
                                            </p>
                                        ) : null}
                                    </div>
                                    <span className="shrink-0 text-[0.8125rem] text-muted-foreground">
                                        {stateLabel(inbound, m.inbounds)}
                                    </span>
                                    {key ? (
                                        <label className="relative inline-flex shrink-0 cursor-pointer items-center has-disabled:cursor-not-allowed has-disabled:opacity-50">
                                            <input
                                                aria-label={m.inbounds.enableLabel(name)}
                                                checked={inbound.enabled}
                                                className="peer sr-only"
                                                disabled={busy}
                                                onChange={(event) =>
                                                    configure({ ...choices, [key]: event.target.checked })
                                                }
                                                role="switch"
                                                type="checkbox"
                                            />
                                            <span
                                                aria-hidden="true"
                                                className="relative h-6 w-10 shrink-0 rounded-full bg-input transition-colors after:absolute after:left-0.5 after:top-0.5 after:size-5 after:rounded-full after:bg-card after:shadow-sm after:transition-transform peer-checked:bg-primary peer-checked:after:translate-x-4 peer-focus-visible:outline-2 peer-focus-visible:outline-offset-2 peer-focus-visible:outline-ring"
                                            />
                                        </label>
                                    ) : null}
                                </li>
                            );
                        })}
                    </ul>
                ) : (
                    <p className="text-sm text-muted-foreground" role="status">
                        {loadError ? m.inbounds.unavailable : m.inbounds.loading}
                    </p>
                )}
                {alert ? (
                    <p className="text-sm text-destructive" role="alert">
                        {alert}
                    </p>
                ) : null}
                <div className="border-t border-border pt-4">
                    <p className="max-w-prose text-[0.8125rem] leading-5 text-muted-foreground">
                        {m.inbounds.tuning}
                    </p>
                </div>
            </CardContent>
        </Card>
    );
}

function ledTone(inbound: InboundStatus): string {
    if (inbound.listening) return "led--ok";
    if (inbound.enabled) return "led--warn";
    return "";
}

function stateLabel(
    inbound: InboundStatus,
    labels: { listening: string; off: string; listenFailed: string },
): string {
    if (inbound.listening) return labels.listening;
    if (inbound.enabled) return labels.listenFailed;
    return labels.off;
}

import type { ServiceState } from "@/types";
import { Badge } from "@/components/ui/badge";
import { messages, useI18n } from "@/i18n";

type StatusTone = "ready" | "working" | "warning" | "danger" | "neutral";

interface StatusDetail {
    label: string;
    message: string;
    tone: StatusTone;
}

const stateTones: Record<ServiceState, StatusTone> = {
    signed_out: "neutral",
    authenticating: "working",
    syncing: "working",
    ready: "ready",
    degraded: "warning",
    expired: "danger",
    error: "danger",
};

export function stateDetail(state: ServiceState): StatusDetail {
    return { ...messages().states[state], tone: stateTones[state] };
}

const toneStyles: Record<StatusTone, { badge: string; led: string }> = {
    ready: { badge: "bg-success text-success-foreground", led: "led--ok" },
    working: {
        badge: "bg-warning text-warning-foreground",
        led: "led--warn led--pulse",
    },
    warning: { badge: "bg-warning text-warning-foreground", led: "led--warn" },
    danger: {
        badge: "bg-destructive/10 text-destructive",
        led: "led--err",
    },
    neutral: { badge: "bg-muted text-muted-foreground", led: "" },
};

export function StatusBadge({ state }: { state: ServiceState }) {
    const { m } = useI18n();
    const tone = toneStyles[stateTones[state]];
    return (
        <Badge
            className={`h-7 gap-2 border-transparent px-3 text-[0.8125rem] ${tone.badge}`}
            variant="outline"
        >
            <span aria-hidden="true" className={`led ${tone.led}`} />
            {m.states[state].label}
        </Badge>
    );
}

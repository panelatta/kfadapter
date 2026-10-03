import type { NodeHealth } from "@/types";

export function formatTime(value?: string): string {
    if (!value) return "—";
    const time = new Date(value);
    return Number.isNaN(time.getTime())
        ? "—"
        : time.toLocaleString(undefined, {
              dateStyle: "medium",
              timeStyle: "short",
          });
}

export function formatCompactTime(value?: string): string {
    if (!value) return "—";
    const time = new Date(value);
    if (Number.isNaN(time.getTime())) return "—";
    const formatted = time.toLocaleString(undefined, {
        month: "short",
        day: "numeric",
        hour: "numeric",
        minute: "2-digit",
    });
    return formatted
        .replace(/,\s*/u, " · ")
        .replace(/\s+(AM|PM)$/u, "\u00a0$1");
}

export interface SubscriptionValidity {
    summary: string;
    endsAt?: string;
}

export function describeSubscriptionValidity(
    subscriptionActive: boolean,
    subscriptionEndsAt?: string,
): SubscriptionValidity {
    if (!subscriptionActive) return { summary: "Free" };
    if (!subscriptionEndsAt)
        return { summary: "Active · End date unavailable" };

    const endTime = new Date(subscriptionEndsAt);
    if (Number.isNaN(endTime.getTime()))
        return { summary: "Active · End date unavailable" };

    const remaining = endTime.getTime() - Date.now();
    const endsAt = formatTime(subscriptionEndsAt);
    if (remaining <= 0) return { summary: "Expired", endsAt };
    if (remaining < 60 * 60 * 1000)
        return { summary: "Active · Valid for less than an hour", endsAt };

    const hours = Math.ceil(remaining / (60 * 60 * 1000));
    if (hours < 24)
        return {
            summary: `Active · Valid for ${hours} ${hours === 1 ? "hour" : "hours"}`,
            endsAt,
        };

    const days = Math.ceil(remaining / (24 * 60 * 60 * 1000));
    return {
        summary: `Active · Valid for ${days} ${days === 1 ? "day" : "days"}`,
        endsAt,
    };
}

export function formatLatency(value?: number): string {
    return typeof value === "number" ? `${value} ms` : "—";
}

export function healthLabel(health: NodeHealth): string {
    return health.slice(0, 1).toUpperCase() + health.slice(1);
}

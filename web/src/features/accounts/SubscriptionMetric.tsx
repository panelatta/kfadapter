import { Metric } from "@/components/Metric";
import { messages, useI18n } from "@/i18n";
import { formatTime } from "@/lib/time";

interface SubscriptionValidity {
    summary: string;
    endsAt?: string;
}

function describeSubscriptionValidity(
    subscriptionActive: boolean,
    subscriptionEndsAt?: string,
): SubscriptionValidity {
    const m = messages().validity;
    if (!subscriptionActive) return { summary: m.free };
    if (!subscriptionEndsAt) return { summary: m.noEndDate };

    const endTime = new Date(subscriptionEndsAt);
    if (Number.isNaN(endTime.getTime())) return { summary: m.noEndDate };

    const remaining = endTime.getTime() - Date.now();
    const endsAt = formatTime(subscriptionEndsAt);
    if (remaining <= 0) return { summary: m.expired, endsAt };
    if (remaining < 60 * 60 * 1000) return { summary: m.underHour, endsAt };

    const hours = Math.ceil(remaining / (60 * 60 * 1000));
    if (hours < 24) return { summary: m.hours(hours), endsAt };

    const days = Math.ceil(remaining / (24 * 60 * 60 * 1000));
    return { summary: m.days(days), endsAt };
}

export function SubscriptionMetric({
    subscriptionActive,
    subscriptionEndsAt,
}: {
    subscriptionActive: boolean;
    subscriptionEndsAt?: string;
}) {
    const { m } = useI18n();
    const validity = describeSubscriptionValidity(
        subscriptionActive,
        subscriptionEndsAt,
    );
    return (
        <Metric
            label={m.validity.title}
            note={
                validity.endsAt ? (
                    <p
                        className="mt-0.5 text-[0.8125rem] text-muted-foreground"
                        title={validity.endsAt}
                    >
                        {m.validity.endsAt(validity.endsAt)}
                    </p>
                ) : null
            }
            value={validity.summary}
        />
    );
}

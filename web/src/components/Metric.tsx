import type { ReactNode } from "react";

export function Metric({
    label,
    note,
    title,
    value,
}: {
    label: string;
    note?: ReactNode;
    title?: string;
    value: string;
}) {
    return (
        <div className="min-w-0">
            <p className="text-[0.8125rem] text-muted-foreground">{label}</p>
            <p
                className="mt-1 break-words text-lg font-semibold leading-snug tabular-nums"
                title={title}
            >
                {value}
            </p>
            {note}
        </div>
    );
}

import type { ReactNode } from "react";
import { CircleAlert } from "lucide-react";

export const selectClassName =
    "flex h-10 w-full rounded-md border border-input bg-card px-3 py-1 text-base text-foreground shadow-xs outline-none transition-[color,border-color,box-shadow] disabled:cursor-not-allowed disabled:opacity-50 md:h-9 md:text-sm focus-visible:border-ring focus-visible:outline-none focus-visible:ring-3 focus-visible:ring-ring/20";

export function Field({
    label,
    name,
    children,
}: {
    label: string;
    name: string;
    children: ReactNode;
}) {
    return (
        <label className="grid gap-1.5" htmlFor={name}>
            <span className="text-[0.8125rem] font-medium text-foreground">
                {label}
            </span>
            {children}
        </label>
    );
}

export function FormError({ message }: { message: string }) {
    return message ? (
        <p
            className="flex items-start gap-2 rounded-md border border-destructive/25 bg-destructive/[0.06] px-3 py-2.5 text-sm leading-5 text-destructive"
            role="alert"
        >
            <CircleAlert aria-hidden="true" className="mt-px size-4 shrink-0" />
            <span className="min-w-0">{message}</span>
        </p>
    ) : null;
}

import { cn } from "@/lib/utils";

/** The wordmark. The glyph is 回, "return", drawn as a square within a square. */
export function BrandMark({
    className,
    version,
}: {
    className?: string;
    version?: string;
}) {
    return (
        <div className={cn("flex min-w-0 items-center gap-2.5", className)}>
            <svg
                aria-hidden="true"
                className="size-7 shrink-0 text-primary"
                fill="none"
                viewBox="0 0 28 28"
            >
                <rect
                    height="24"
                    rx="6"
                    stroke="currentColor"
                    strokeWidth="2.5"
                    width="24"
                    x="2"
                    y="2"
                />
                <rect fill="currentColor" height="8" rx="1.5" width="8" x="10" y="10" />
            </svg>
            <span className="min-w-0">
                <span className="block truncate text-[0.9375rem] font-semibold leading-5 tracking-[-0.01em]">
                    chinadapter
                </span>
                {version ? (
                    <span className="block truncate text-xs leading-4 text-muted-foreground">
                        {version}
                    </span>
                ) : null}
            </span>
        </div>
    );
}

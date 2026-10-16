import { locales, useI18n } from "@/i18n";
import { cn } from "@/lib/utils";

export function LanguageSwitch({ className }: { className?: string }) {
    const { locale, m, setLocale } = useI18n();
    return (
        <div
            aria-label={m.language.label}
            className={cn(
                "inline-flex shrink-0 rounded-md border border-border bg-muted p-0.5",
                className,
            )}
            role="group"
        >
            {locales.map((option) => (
                <button
                    aria-pressed={locale === option}
                    className={cn(
                        "rounded-[0.3125rem] px-2.5 py-0.5 text-[0.8125rem] font-medium text-muted-foreground transition-colors hover:text-foreground",
                        locale === option &&
                            "bg-card text-foreground shadow-xs",
                    )}
                    key={option}
                    lang={option === "zh" ? "zh-CN" : "en"}
                    onClick={() => setLocale(option)}
                    type="button"
                >
                    {m.language[option]}
                </button>
            ))}
        </div>
    );
}

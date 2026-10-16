import { useSyncExternalStore } from "react";
import { en } from "./en";
import type { Messages } from "./en";
import { zh } from "./zh";

export type { Messages } from "./en";
export type Locale = "en" | "zh";

export const locales: readonly Locale[] = ["zh", "en"];

const catalogs: Record<Locale, Messages> = { en, zh };
const listeners = new Set<() => void>();

function preferredLocale(): Locale {
    const languages =
        typeof navigator === "undefined"
            ? []
            : navigator.languages?.length
              ? navigator.languages
              : [navigator.language];
    return languages.some((language) => /^zh\b/iu.test(language ?? ""))
        ? "zh"
        : "en";
}

// The console never writes browser storage, so a switch lasts until the page
// reloads; each load starts from the browser's language.
let current: Locale = preferredLocale();

function applyDocumentLanguage(locale: Locale) {
    if (typeof document !== "undefined")
        document.documentElement.lang = locale === "zh" ? "zh-CN" : "en";
}

applyDocumentLanguage(current);

export function getLocale(): Locale {
    return current;
}

/** The catalog for the current locale; for code outside React components. */
export function messages(): Messages {
    return catalogs[current];
}

/** The BCP 47 tag used for dates and collation in the current locale. */
export function intlLocale(locale: Locale = current): string {
    return locale === "zh" ? "zh-CN" : "en-US";
}

export function setLocale(locale: Locale): void {
    if (locale === current) return;
    current = locale;
    applyDocumentLanguage(locale);
    for (const listener of listeners) listener();
}

function subscribe(listener: () => void): () => void {
    listeners.add(listener);
    return () => listeners.delete(listener);
}

export function useI18n() {
    const locale = useSyncExternalStore(subscribe, getLocale, getLocale);
    return { locale, m: catalogs[locale], setLocale };
}

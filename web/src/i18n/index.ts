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

/*
 * The language preference is the only thing the console keeps in browser
 * storage: it is not a secret and grants nothing. The key stays a literal at
 * both call sites so the security test can allow exactly these two accesses
 * in the production bundle and reject any other storage use.
 */
function savedLocale(): Locale | null {
    try {
        const value = window.localStorage.getItem("chinadapter.locale");
        return value === "zh" || value === "en" ? value : null;
    } catch {
        return null;
    }
}

function saveLocale(locale: Locale): void {
    try {
        window.localStorage.setItem("chinadapter.locale", locale);
    } catch {
        // Without storage the choice lasts until the page reloads.
    }
}

let current: Locale = savedLocale() ?? preferredLocale();

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

/** Switches the language for this page without remembering it. */
export function setLocale(locale: Locale): void {
    if (locale === current) return;
    current = locale;
    applyDocumentLanguage(locale);
    for (const listener of listeners) listener();
}

/** The operator's explicit choice: switches and remembers it. */
export function chooseLocale(locale: Locale): void {
    saveLocale(locale);
    setLocale(locale);
}

function subscribe(listener: () => void): () => void {
    listeners.add(listener);
    return () => listeners.delete(listener);
}

export function useI18n() {
    const locale = useSyncExternalStore(subscribe, getLocale, getLocale);
    return { locale, m: catalogs[locale], setLocale: chooseLocale };
}

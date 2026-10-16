import type { ServiceState } from "@/types";
import { messages } from "@/i18n";

export const providerLoginStates: Partial<Record<ServiceState, true>> = {
    signed_out: true,
    expired: true,
};

const providerLabels: Record<string, string> = {
    kuaifan: "KuaiFan",
    quickfox: "QuickFox",
    transocks: "Transocks",
    baidu: "Baidu Proxy",
};

export function providerLabel(provider: string): string {
    return providerLabels[provider] ?? provider;
}

/** Credential field labels arrive from the service in English. */
export function fieldLabel(label: string): string {
    const labels = messages().providers.fieldLabels;
    return Object.hasOwn(labels, label) ? labels[label] : label;
}

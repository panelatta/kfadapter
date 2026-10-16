import type { AccountSummary } from "@/types";
import { messages } from "@/i18n";
import { formatCompactTime } from "@/lib/time";

export type LampTone = "ok" | "warn" | "err" | "off";

export const lampClass: Record<LampTone, string> = {
    ok: "led--ok",
    warn: "led--warn",
    err: "led--err",
    off: "",
};

export interface AccountIssue {
    tone: "warn" | "err";
    /** Short cause, e.g. "Can't reach provider". */
    summary: string;
    /** What happens next or what the operator should do. */
    action: string;
}

export function accountIssue(account?: AccountSummary): AccountIssue | null {
    if (!account) return null;
    const m = messages().health;
    const problem = account.refreshProblem ?? "";
    const cause = Object.hasOwn(m.causes, problem)
        ? m.causes[problem]
        : m.refreshFailed;
    switch (account.refreshStatus) {
        case "needs_relogin":
            return {
                tone: "err",
                summary: cause,
                action: m.reloginAction,
            };
        case "refresh_failing":
            return {
                tone: "warn",
                summary: cause,
                action: account.nextRefreshAt
                    ? m.retryingAt(formatCompactTime(account.nextRefreshAt))
                    : m.retrying,
            };
        default:
            return null;
    }
}

export function accountTone(account?: AccountSummary): LampTone {
    if (!account) return "off";
    return accountIssue(account)?.tone ?? "ok";
}

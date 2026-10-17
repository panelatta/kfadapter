import { ApiError } from "@/api";
import { messages } from "@/i18n";
import type { Messages } from "@/i18n";

export function isSessionProblem(error: unknown): boolean {
    return (
        error instanceof ApiError &&
        (error.problem.status === 401 || error.problem.status === 403)
    );
}

// Keyed by the backend's problem codes. Anything else falls back to the
// problem's detail or title.
const problemMessages: Record<string, keyof Messages["errors"]> = {
    session_revocation_pending: "sessionRevocationPending",
    setup_requires_loopback: "setupRequiresLoopback",
    access_rate_limited: "tooManyAttempts",
    login_rate_limited: "tooManyAttempts",
    access_in_progress: "accessInProgress",
    login_in_progress: "loginInProgress",
    operation_in_progress: "operationInProgress",
    invalid_access_token: "invalidAccessToken",
    invalid_next_access_token: "invalidNextAccessToken",
    access_initialized: "accessInitialized",
    login_rejected: "loginRejected",
    device_limit: "deviceLimit",
    login_unavailable: "loginUnavailable",
    account_exists: "accountExists",
    invalid_smart_policy: "invalidSmartPolicy",
    smart_config_failed: "smartConfigFailed",
    smart_probe_unavailable: "smartProbeUnavailable",
    smart_proxy_unavailable: "smartProxyUnavailable",
    inbound_listen_failed: "inboundListenFailed",
    invalid_inbounds: "invalidInbounds",
    inbounds_config_failed: "inboundsConfigFailed",
};

export function describeError(error: unknown): string {
    if (error instanceof ApiError) {
        const { code } = error.problem;
        if (Object.hasOwn(problemMessages, code))
            return messages().errors[problemMessages[code]];
        return error.problem.detail || error.problem.title;
    }
    return messages().errors.requestFailed;
}

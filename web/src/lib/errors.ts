import { ApiError } from "@/api";

export function isSessionProblem(error: unknown): boolean {
    return (
        error instanceof ApiError &&
        (error.problem.status === 401 || error.problem.status === 403)
    );
}

const problemMessages: Record<string, string> = {
    access_rate_limited: "Too many attempts. Wait a moment and try again.",
    login_rate_limited: "Too many attempts. Wait a moment and try again.",
    access_in_progress: "Another attempt is still being checked. Try again in a moment.",
    login_in_progress: "Another sign-in is still in progress. Try again in a moment.",
    operation_in_progress: "Another account operation is still running. Try again in a moment.",
    invalid_access_token: "That access token was not accepted.",
    access_initialized: "Console access has already been created. Unlock it with your access token.",
    login_rejected: "The provider rejected that email or password.",
    login_unavailable: "The provider could not be reached. Try again later.",
    account_exists: "That provider already has a connected account. Remove it first to switch accounts.",
};

export function describeError(error: unknown): string {
    if (error instanceof ApiError) {
        const message = problemMessages[error.problem.code ?? ""];
        if (message) return message;
        return error.problem.detail || error.problem.title;
    }
    return "The local service did not complete that request.";
}

export function probeFailureLabel(error: unknown): string {
    if (!(error instanceof ApiError)) return "Failed";
    switch (error.problem.code) {
        case "node_ineligible":
            return "Ineligible";
        case "node_not_found":
        case "node_snapshot_unavailable":
            return "Unavailable";
        case "probe_busy":
            return "Busy";
        case "stale_probe":
            return "Changed";
        case "tcp_probe_timeout":
            return "Timeout";
        case "tcp_probe_failed":
        case "probe_failed":
            return "Unreachable";
        default:
            return "Failed";
    }
}

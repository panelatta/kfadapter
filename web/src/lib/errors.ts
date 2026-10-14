import { ApiError } from "@/api";

export function isSessionProblem(error: unknown): boolean {
    return (
        error instanceof ApiError &&
        (error.problem.status === 401 || error.problem.status === 403)
    );
}

const tooManyAttempts = "Too many attempts. Wait a moment and try again.";

// Keyed by the backend's problem codes. Anything else falls back to the
// problem's detail or title.
const problemMessages: Record<string, string> = {
    session_revocation_pending:
        "The console is locked now, but an old session could become valid again after the service restarts. Repair persistent storage, then unlock and lock the console again to retry.",
    setup_requires_loopback:
        "For safety, the first access token can only be created from the device running chinadapter. Open this console at http://127.0.0.1 on that device, for example through an SSH port forward.",
    access_rate_limited: tooManyAttempts,
    login_rate_limited: tooManyAttempts,
    access_in_progress:
        "Another attempt is still being checked. Try again in a moment.",
    login_in_progress:
        "Another sign-in is still in progress. Try again in a moment.",
    operation_in_progress:
        "Another account operation is still running. Try again in a moment.",
    invalid_access_token: "That access token was not accepted.",
    invalid_next_access_token:
        "Use a different access token of 16–128 bytes.",
    access_initialized:
        "Console access has already been created. Unlock it with your access token.",
    login_rejected: "The provider rejected that email or password.",
    login_unavailable: "The provider could not be reached. Try again later.",
    account_exists:
        "That provider already has a connected account. Remove it first to switch accounts.",
};

export function describeError(error: unknown): string {
    if (error instanceof ApiError) {
        const { code } = error.problem;
        if (Object.hasOwn(problemMessages, code)) return problemMessages[code];
        return error.problem.detail || error.problem.title;
    }
    return "The local service did not complete that request.";
}

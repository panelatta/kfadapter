import { useCallback, useEffect, useRef, useState } from "react";
import { ApiClient, ApiError } from "@/api";
import type { NodeRecord, StatusResponse } from "@/types";
import { providerLoginStates } from "@/features/accounts/providers";
import { useProviderActions } from "@/features/accounts/useProviderActions";
import { useConsoleEvents } from "@/features/events/useConsoleEvents";
import { messages } from "@/i18n";
import { describeError, isSessionProblem } from "@/lib/errors";
import { replaceRoute, routeFromLocation } from "./session";
import type { Route, SessionState } from "./session";

export function useConsoleSession(providedApi?: ApiClient) {
    const [api] = useState<ApiClient>(() => providedApi ?? new ApiClient());
    const [route, setRoute] = useState<Route>(routeFromLocation);
    const [session, setSession] = useState<SessionState>("checking");
    const [setupAllowed, setSetupAllowed] = useState(true);
    const [status, setStatus] = useState<StatusResponse | null>(null);
    const [nodes, setNodes] = useState<NodeRecord[]>([]);
    const [subscriptionURL, setSubscriptionURL] = useState("");
    const [loadError, setLoadError] = useState("");
    const [refreshing, setRefreshing] = useState(false);
    const sessionRevision = useRef(0);
    const lockPending = useRef(false);
    const loadRevision = useRef(0);
    const reloadWork = useRef({
        scheduled: false,
        running: false,
        queued: false,
        revision: 0,
    });

    const getSessionRevision = useCallback(() => sessionRevision.current, []);

    const clearProviderData = useCallback(() => {
        setStatus(null);
        setNodes([]);
        setSubscriptionURL("");
        setRefreshing(false);
    }, []);

    const clearConsoleData = useCallback(() => {
        clearProviderData();
        setLoadError("");
    }, [clearProviderData]);

    const loseAccessSession = useCallback(() => {
        sessionRevision.current += 1;
        reloadWork.current.queued = false;
        clearConsoleData();
        setSession("access_login");
        replaceRoute("/");
    }, [clearConsoleData]);

    const loadAuthenticatedData =
        useCallback(async (): Promise<StatusResponse | null> => {
            const revision = sessionRevision.current;
            const request = ++loadRevision.current;
            // Explicit actions can overlap an SSE reload. Only the newest load
            // may publish data or errors, including failures from an old session.
            const isCurrent = () =>
                revision === sessionRevision.current &&
                request === loadRevision.current;
            try {
                const nextStatus = await api.status();
                if (!isCurrent()) return null;

                if (providerLoginStates[nextStatus.state]) {
                    setStatus(nextStatus);
                    setNodes([]);
                    setSubscriptionURL("");
                    setLoadError("");
                    return nextStatus;
                }

                setStatus(nextStatus);
                setLoadError("");

                // Nodes and the subscription URL load independently: one failing
                // must not hide the other.
                const [nextNodes, nextSubscription] = await Promise.allSettled([
                    api.nodes(),
                    api.subscriptionURL(),
                ]);
                if (!isCurrent()) return null;

                for (const result of [nextNodes, nextSubscription]) {
                    if (
                        result.status === "rejected" &&
                        isSessionProblem(result.reason)
                    )
                        throw result.reason;
                }
                if (nextNodes.status === "fulfilled")
                    setNodes(nextNodes.value.nodes);
                if (nextSubscription.status === "fulfilled")
                    setSubscriptionURL(nextSubscription.value.url);
                const failure = [nextNodes, nextSubscription].find(
                    (result) => result.status === "rejected",
                );
                if (failure?.status === "rejected")
                    setLoadError(describeError(failure.reason));
                return nextStatus;
            } catch (error) {
                if (!isCurrent()) return null;
                if (isSessionProblem(error)) {
                    loseAccessSession();
                    return null;
                }
                setLoadError(describeError(error));
                return null;
            }
        }, [api, loseAccessSession]);

    const queueAuthenticatedReload = useCallback(() => {
        const work = reloadWork.current;
        work.queued = true;
        work.revision = sessionRevision.current;
        if (work.scheduled || work.running) return;

        work.scheduled = true;
        queueMicrotask(async () => {
            work.scheduled = false;
            // A reload queued while another is running (including one queued for
            // a newer session) is picked up by this loop rather than dropped.
            while (work.queued && work.revision === sessionRevision.current) {
                work.queued = false;
                work.running = true;
                try {
                    await loadAuthenticatedData();
                } finally {
                    work.running = false;
                }
            }
        });
    }, [loadAuthenticatedData]);

    const retryLoad = useCallback(() => {
        setLoadError("");
        queueAuthenticatedReload();
    }, [queueAuthenticatedReload]);

    useEffect(() => {
        const updateRoute = () => setRoute(routeFromLocation());
        window.addEventListener("popstate", updateRoute);
        return () => window.removeEventListener("popstate", updateRoute);
    }, []);

    useEffect(() => {
        if (window.location.pathname !== route) replaceRoute(route);
    }, [route]);

    useEffect(() => {
        if (session !== "available" && route !== "/") replaceRoute("/");
    }, [route, session]);

    useEffect(() => {
        // Route only on a loaded status: an unknown status is still loading or
        // failed, and must not send a signed-in user to the account form.
        if (session !== "available" || !status) return;
        const needsProviderLogin = Boolean(providerLoginStates[status.state]);
        if (needsProviderLogin && route !== "/signin") replaceRoute("/signin");
        if (!needsProviderLogin && route === "/signin") replaceRoute("/");
    }, [route, session, status]);

    useEffect(() => {
        let active = true;
        api.accessStatus()
            .then((access) => {
                if (!active) return;
                if (!access.initialized) {
                    setSetupAllowed(access.setupAllowed !== false);
                    setSession("setup");
                    return;
                }
                if (!access.authenticated) {
                    setSession("access_login");
                    return;
                }
                sessionRevision.current += 1;
                setSession("available");
                queueAuthenticatedReload();
            })
            .catch((error: unknown) => {
                if (!active) return;
                setLoadError(describeError(error));
                setSession("access_login");
            });
        return () => {
            active = false;
        };
    }, [api, queueAuthenticatedReload]);

    useConsoleEvents({
        api,
        getSessionRevision,
        queueAuthenticatedReload,
        session,
        setNodes,
    });

    const completeAccess = useCallback(() => {
        sessionRevision.current += 1;
        setLoadError("");
        setSession("available");
        replaceRoute("/");
        queueAuthenticatedReload();
    }, [queueAuthenticatedReload]);

    const setupAccess = async (token: string): Promise<void> => {
        await api.setupAccess(token);
        completeAccess();
    };

    const loginAccess = async (token: string): Promise<void> => {
        await api.loginAccess(token);
        completeAccess();
    };

    // The service revokes every browser session and issues this browser a
    // replacement one, so a success restarts the authenticated session.
    const changeAccessToken = async (
        current: string,
        next: string,
    ): Promise<void> => {
        const revision = sessionRevision.current;
        try {
            await api.changeAccessToken(current, next);
        } catch (error) {
            if (revision === sessionRevision.current && error instanceof ApiError) {
                const { code, status: httpStatus } = error.problem;
                if (code === "not_authenticated" || httpStatus === 403) {
                    loseAccessSession();
                } else if (code === "session_unavailable") {
                    // The token changed but the replacement session failed.
                    loseAccessSession();
                    setLoadError(messages().access.tokenChangedSessionLost);
                }
            }
            throw error;
        }
        if (revision === sessionRevision.current) completeAccess();
    };

    const lockConsole = async (): Promise<void> => {
        if (lockPending.current) return;
        lockPending.current = true;
        const revision = ++sessionRevision.current;
        reloadWork.current.queued = false;
        clearConsoleData();
        // Wait for the logout response before allowing a new login: that
        // response clears the session cookie, and must not clear a newer one.
        setSession("locking");
        replaceRoute("/");
        try {
            await api.lockConsole();
        } catch (error) {
            // A storage failure locks this process but must remain visible:
            // its revoked session could be restored after a service restart.
            if (
                revision === sessionRevision.current &&
                !isSessionProblem(error)
            )
                setLoadError(
                    error instanceof ApiError &&
                        error.problem.code === "session_revocation_pending"
                        ? describeError(error)
                        : messages().access.unconfirmedLock,
                );
        } finally {
            lockPending.current = false;
            if (revision === sessionRevision.current) setSession("access_login");
        }
    };

    const providerActions = useProviderActions({
        api,
        getSessionRevision,
        loadAuthenticatedData,
        loseAccessSession,
        setLoadError,
        setRefreshing,
        status,
    });

    return {
        api,
        route,
        session,
        setupAllowed,
        status,
        nodes,
        subscriptionURL,
        loadError,
        refreshing,
        setupAccess,
        loginAccess,
        changeAccessToken,
        loseAccessSession,
        lockConsole,
        retryLoad,
        getSessionRevision,
        setNodes,
        ...providerActions,
    };
}

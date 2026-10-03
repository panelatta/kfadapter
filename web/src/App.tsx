import { useCallback, useEffect, useRef, useState } from "react";
import { ApiClient } from "@/api";
import { TooltipProvider } from "@/components/ui/tooltip";
import { providerLoginStates, groupNameCollator, applyProbeResult } from "@/lib/console";
import { routeFromLocation, replaceRoute, type Route } from "@/lib/routes";
import { isSessionProblem, describeError } from "@/lib/errors";
import { ServiceErrorCard, LoadingCard, AccessTokenCard, ProviderLoginCard } from "@/components/AuthCards";
import { StatusConsole } from "@/components/StatusConsole";
import type { EventMessage, NodeRecord, StatusResponse } from "@/types";

type SessionState = "checking" | "setup" | "access_login" | "available";

interface AppProps {
    api?: ApiClient;
}

export function App({ api: providedApi }: AppProps) {
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
    const reloadWork = useRef({
        scheduled: false,
        running: false,
        queued: false,
        revision: 0,
    });
    const nextSSEFailureReloadAt = useRef(0);

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
        api.clearSession();
        clearConsoleData();
        setSession("access_login");
        replaceRoute("/");
    }, [api, clearConsoleData]);

    const loadAuthenticatedData =
        useCallback(async (): Promise<StatusResponse | null> => {
            const revision = sessionRevision.current;
            try {
                const nextStatus = await api.status();
                if (revision !== sessionRevision.current) return null;

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
                if (revision !== sessionRevision.current) return null;

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
                if (revision !== sessionRevision.current) return null;
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

    useEffect(() => {
        if (session !== "available") return;
        let active = true;
        const closeEvents = api.events(
            (event: EventMessage) => {
                if (!active) return;
                if (event.type === "probe") {
                    setNodes((previous) => applyProbeResult(previous, event));
                    return;
                }
                queueAuthenticatedReload();
            },
            () => {
                if (!active) return;
                const now = Date.now();
                if (now < nextSSEFailureReloadAt.current) return;
                nextSSEFailureReloadAt.current = now + 1000;
                queueAuthenticatedReload();
            },
        );
        return () => {
            active = false;
            closeEvents();
        };
    }, [api, queueAuthenticatedReload, session]);

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

    const loginAccount = async (
        provider: string,
        account: string,
        password: string,
    ): Promise<void> => {
        const revision = sessionRevision.current;
        await api.login(provider, account, password);
        await loadAuthenticatedData();
        if (revision === sessionRevision.current) replaceRoute("/");
    };

    const mutateProvider = async (
        provider: string,
        operation: (provider: string) => Promise<void>,
    ): Promise<void> => {
        const revision = sessionRevision.current;
        setRefreshing(true);
        try {
            await operation(provider);
            if (revision === sessionRevision.current)
                await loadAuthenticatedData();
        } catch (error) {
            if (revision === sessionRevision.current && isSessionProblem(error))
                loseAccessSession();
            throw error;
        } finally {
            if (revision === sessionRevision.current) setRefreshing(false);
        }
    };

    const refreshProvider = (provider: string) =>
        mutateProvider(provider, (id) => api.refresh(id));

    const signOutAll = async (): Promise<void> => {
        const revision = sessionRevision.current;
        const linkedProviders = Object.keys(status?.accounts ?? {}).sort(
            groupNameCollator.compare,
        );
        if (linkedProviders.length === 0) return;

        setRefreshing(true);
        setLoadError("");
        try {
            for (const provider of linkedProviders)
                await api.logoutAccount(provider);
            if (revision === sessionRevision.current)
                await loadAuthenticatedData();
        } catch (error) {
            if (revision !== sessionRevision.current) return;
            if (isSessionProblem(error)) {
                loseAccessSession();
                return;
            }
            setLoadError(describeError(error));
        } finally {
            if (revision === sessionRevision.current) setRefreshing(false);
        }
    };
    const removeProvider = (provider: string) =>
        mutateProvider(provider, (id) => api.logoutAccount(id));

    const lockConsole = async (): Promise<void> => {
        const revision = ++sessionRevision.current;
        reloadWork.current.queued = false;
        clearConsoleData();
        setSession("access_login");
        replaceRoute("/");
        try {
            await api.lockConsole();
        } catch (error) {
            // The service revokes a session even when it cannot persist that yet,
            // so only an unreachable service leaves this browser session alive.
            if (
                revision === sessionRevision.current &&
                !isSessionProblem(error)
            )
                setLoadError(
                    "The console could not confirm the lock with the service. If this browser is shared, reload and lock again.",
                );
        } finally {
            api.clearSession();
        }
    };

    const refreshAll = async (): Promise<void> => {
        const revision = sessionRevision.current;
        setRefreshing(true);
        setLoadError("");
        try {
            await api.refresh("");
            if (revision === sessionRevision.current)
                await loadAuthenticatedData();
        } catch (error) {
            if (revision !== sessionRevision.current) return;
            if (isSessionProblem(error)) {
                loseAccessSession();
                return;
            }
            setLoadError(describeError(error));
        } finally {
            if (revision === sessionRevision.current) setRefreshing(false);
        }
    };

    const getSessionRevision = useCallback(() => sessionRevision.current, []);

    let content;
    if (session === "checking") {
        content = <LoadingCard />;
    } else if (session === "setup") {
        content = (
            <AccessTokenCard
                mode="setup"
                onSubmit={setupAccess}
                error={loadError}
                setupAllowed={setupAllowed}
            />
        );
    } else if (session === "access_login") {
        content = (
            <AccessTokenCard
                mode="login"
                onSubmit={loginAccess}
                error={loadError}
            />
        );
    } else if (!status) {
        content = loadError ? (
            <ServiceErrorCard
                message={loadError}
                onRetry={queueAuthenticatedReload}
            />
        ) : (
            <LoadingCard />
        );
    } else if (route === "/signin" || providerLoginStates[status.state]) {
        content = (
            <ProviderLoginCard
                providers={status?.providers ?? []}
                onSubmit={loginAccount}
                error={loadError}
            />
        );
    } else {
        content = (
            <StatusConsole
                api={api}
                getSessionRevision={getSessionRevision}
                loadError={loadError}
                nodes={nodes}
                onAccessLost={loseAccessSession}
                onLockConsole={lockConsole}
                onNodesChange={setNodes}
                onLoginAccount={loginAccount}
                onRefreshAll={refreshAll}
                onRefreshProvider={refreshProvider}
                onRemoveProvider={removeProvider}
                onSignOutAll={signOutAll}
                refreshing={refreshing}
                status={status}
                subscriptionURL={subscriptionURL}
            />
        );
    }

    return <TooltipProvider delayDuration={150}>{content}</TooltipProvider>;
}

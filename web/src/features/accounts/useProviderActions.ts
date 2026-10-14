import type { Dispatch, SetStateAction } from "react";
import type { ApiClient } from "@/api";
import type { StatusResponse } from "@/types";
import { replaceRoute } from "@/features/access/session";
import { describeError, isSessionProblem } from "@/lib/errors";
import { groupNameCollator } from "@/lib/groups";

export function useProviderActions({
    api,
    getSessionRevision,
    loadAuthenticatedData,
    loseAccessSession,
    setLoadError,
    setRefreshing,
    status,
}: {
    api: ApiClient;
    getSessionRevision: () => number;
    loadAuthenticatedData: () => Promise<StatusResponse | null>;
    loseAccessSession: () => void;
    setLoadError: Dispatch<SetStateAction<string>>;
    setRefreshing: Dispatch<SetStateAction<boolean>>;
    status: StatusResponse | null;
}) {
    const loginAccount = async (
        provider: string,
        account: string,
        password: string,
    ): Promise<void> => {
        const revision = getSessionRevision();
        await api.login(provider, account, password);
        // The console may have been locked while the provider was answering.
        if (revision !== getSessionRevision()) return;
        await loadAuthenticatedData();
        if (revision === getSessionRevision()) replaceRoute("/");
    };

    const mutateProvider = async (
        provider: string,
        operation: (provider: string) => Promise<void>,
    ): Promise<void> => {
        const revision = getSessionRevision();
        setRefreshing(true);
        try {
            await operation(provider);
            if (revision === getSessionRevision())
                await loadAuthenticatedData();
        } catch (error) {
            if (revision === getSessionRevision() && isSessionProblem(error))
                loseAccessSession();
            throw error;
        } finally {
            if (revision === getSessionRevision()) setRefreshing(false);
        }
    };

    const refreshProvider = (provider: string) =>
        mutateProvider(provider, (id) => api.refresh(id));

    const signOutAll = async (): Promise<void> => {
        const revision = getSessionRevision();
        const linkedProviders = Object.keys(status?.accounts ?? {}).sort(
            groupNameCollator.compare,
        );
        if (linkedProviders.length === 0) return;

        setRefreshing(true);
        setLoadError("");
        try {
            for (const provider of linkedProviders)
                await api.logoutAccount(provider);
            if (revision === getSessionRevision())
                await loadAuthenticatedData();
        } catch (error) {
            if (revision !== getSessionRevision()) return;
            if (isSessionProblem(error)) {
                loseAccessSession();
                return;
            }
            setLoadError(describeError(error));
        } finally {
            if (revision === getSessionRevision()) setRefreshing(false);
        }
    };
    const removeProvider = (provider: string) =>
        mutateProvider(provider, (id) => api.logoutAccount(id));

    const refreshAll = async (): Promise<void> => {
        const revision = getSessionRevision();
        setRefreshing(true);
        setLoadError("");
        try {
            await api.refresh("");
            if (revision === getSessionRevision())
                await loadAuthenticatedData();
        } catch (error) {
            if (revision !== getSessionRevision()) return;
            if (isSessionProblem(error)) {
                loseAccessSession();
                return;
            }
            setLoadError(describeError(error));
        } finally {
            if (revision === getSessionRevision()) setRefreshing(false);
        }
    };

    return { loginAccount, refreshProvider, removeProvider, signOutAll, refreshAll };
}

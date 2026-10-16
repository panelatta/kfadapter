import { useState } from "react";
import type { StatusResponse } from "@/types";
import { describeError, isSessionProblem } from "@/lib/errors";

export interface AccountViews {
    activeProvider: string;
    confirmRemoval: string;
    providerBusy: boolean;
    providerError: string;
    runProviderAction: (action: () => Promise<void>) => Promise<void>;
    selectProvider: (provider: string) => void;
    setConfirmRemoval: (provider: string) => void;
}

export function useAccountViews(status: StatusResponse, resetSubscriptionFilters: () => void): AccountViews {
    const [selectedProvider, setActiveProvider] = useState("all");
    const [providerError, setProviderError] = useState("");
    const [providerBusy, setProviderBusy] = useState(false);
    const [confirmRemoval, setConfirmRemoval] = useState("");
    // A provider that disappears from the status falls back to the overview.
    const activeProvider =
        selectedProvider === "all" ||
        status.providers.includes(selectedProvider)
            ? selectedProvider
            : "all";

    const selectProvider = (provider: string) => {
        setActiveProvider(provider);
        setProviderError("");
        setConfirmRemoval("");
        if (provider !== "all") resetSubscriptionFilters();
    };

    const runProviderAction = async (action: () => Promise<void>) => {
        setProviderBusy(true);
        setProviderError("");
        try {
            await action();
            setConfirmRemoval("");
        } catch (error) {
            if (!isSessionProblem(error))
                setProviderError(describeError(error));
        } finally {
            setProviderBusy(false);
        }
    };

    return {
        activeProvider,
        confirmRemoval,
        providerBusy,
        providerError,
        runProviderAction,
        selectProvider,
        setConfirmRemoval,
    };
}

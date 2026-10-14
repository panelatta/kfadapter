import { useState } from "react";
import type { StatusResponse } from "@/types";
import { describeError, isSessionProblem } from "@/lib/errors";

export function useAccountViews(status: StatusResponse, resetSubscriptionFilters: () => void) {
    const [selectedProvider, setActiveProvider] = useState("all");
    const [providerError, setProviderError] = useState("");
    const [providerBusy, setProviderBusy] = useState(false);
    const [confirmRemoval, setConfirmRemoval] = useState("");
    const [confirmSignOutAll, setConfirmSignOutAll] = useState(false);
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
        setConfirmSignOutAll(false);
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
        confirmSignOutAll,
        providerBusy,
        providerError,
        runProviderAction,
        selectProvider,
        setConfirmRemoval,
        setConfirmSignOutAll,
    };
}

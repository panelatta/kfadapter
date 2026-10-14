import { useEffect, useRef } from "react";
import type { Dispatch, SetStateAction } from "react";
import type { ApiClient } from "@/api";
import type { EventMessage, NodeRecord } from "@/types";
import type { SessionState } from "@/features/access/session";
import { applyProbeResult } from "@/features/nodes/health";

export function useConsoleEvents({
    api,
    getSessionRevision,
    queueAuthenticatedReload,
    session,
    setNodes,
}: {
    api: ApiClient;
    getSessionRevision: () => number;
    queueAuthenticatedReload: () => void;
    session: SessionState;
    setNodes: Dispatch<SetStateAction<NodeRecord[]>>;
}) {
    const nextSSEFailureReloadAt = useRef(0);

    useEffect(() => {
        if (session !== "available") return;
        let active = true;
        // Callbacks from a stream opened for an earlier browser session must
        // not touch the data of the current one.
        const revision = getSessionRevision();
        const isCurrentSession = () =>
            active && revision === getSessionRevision();
        const closeEvents = api.events(
            (event: EventMessage) => {
                if (!isCurrentSession()) return;
                if (event.type === "probe") {
                    setNodes((previous) => applyProbeResult(previous, event));
                    return;
                }
                queueAuthenticatedReload();
            },
            () => {
                if (!isCurrentSession()) return;
                const now = Date.now();
                if (now < nextSSEFailureReloadAt.current) return;
                nextSSEFailureReloadAt.current = now + 1000;
                queueAuthenticatedReload();
            },
            () => {
                // The server does not replay events missed while disconnected,
                // so every (re)opened stream starts with an authoritative load.
                if (isCurrentSession()) queueAuthenticatedReload();
            },
        );
        return () => {
            active = false;
            closeEvents();
        };
    }, [api, getSessionRevision, queueAuthenticatedReload, session, setNodes]);
}

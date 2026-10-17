import { useEffect, useRef, useState } from "react";
import type { ApiClient } from "@/api";
import type { SmartProxyDetails, SmartProxyStatus } from "@/types";
import { messages } from "@/i18n";
import { copyText } from "@/lib/clipboard";
import { describeError, isSessionProblem } from "@/lib/errors";

const pollIntervalMs = 5000;

function profileFor(details: SmartProxyDetails, protocol: string) {
    return details.profiles?.find((profile) => profile.protocol === protocol);
}

function protocolName(protocol: string): string {
    return messages().inbounds.names[protocol] ?? protocol;
}

/**
 * Polls the smart proxy status and runs one user action at a time. Responses
 * are ignored once the browser session changes, the card unmounts, or a newer
 * operation has started, so an older snapshot or failure never overwrites a
 * completed change.
 */
export function useSmartProxy({
    api,
    getSessionRevision,
    onAccessLost,
}: {
    api: ApiClient;
    getSessionRevision: () => number;
    onAccessLost: () => void;
}) {
    const [status, setStatus] = useState<SmartProxyStatus | null>(null);
    const [error, setError] = useState("");
    const [loadError, setLoadError] = useState("");
    const [message, setMessage] = useState("");
    const [busy, setBusy] = useState(false);
    const operation = useRef(0);
    const operationPending = useRef(false);
    const mounted = useRef(false);
    const sessionRevision = useRef<number | null>(null);

    useEffect(() => {
        mounted.current = true;
        let active = true;
        let timer: ReturnType<typeof setTimeout> | undefined;
        const revision = getSessionRevision();
        sessionRevision.current = revision;
        const isCurrentSession = () =>
            active && mounted.current && revision === getSessionRevision();
        const load = async () => {
            if (!isCurrentSession()) return;
            const sequence = operation.current;
            const isCurrent = () =>
                isCurrentSession() && sequence === operation.current;
            try {
                // A poll started during a write must not publish its older
                // snapshot or revoke access while the write is completing.
                if (operationPending.current) return;
                const next = await api.smartProxy();
                if (isCurrent()) {
                    setStatus(next);
                    setLoadError("");
                    setBusy(false);
                }
            } catch (cause) {
                if (isCurrent()) {
                    if (isSessionProblem(cause)) {
                        mounted.current = false;
                        onAccessLost();
                    } else setLoadError(describeError(cause));
                }
            } finally {
                // Chain the next poll only after this one settled, so slow
                // responses never pile up.
                if (isCurrentSession())
                    timer = setTimeout(() => void load(), pollIntervalMs);
            }
        };
        void load();
        return () => {
            active = false;
            mounted.current = false;
            operation.current += 1;
            operationPending.current = false;
            clearTimeout(timer);
        };
    }, [api, getSessionRevision, onAccessLost]);

    /** Runs a single-flight action; a second action while one is pending is dropped. */
    const run = async (work: (isCurrent: () => boolean) => Promise<void>) => {
        const revision = getSessionRevision();
        if (
            !mounted.current ||
            operationPending.current ||
            revision !== sessionRevision.current
        )
            return;
        const sequence = ++operation.current;
        operationPending.current = true;
        const isCurrent = () =>
            mounted.current &&
            revision === getSessionRevision() &&
            sequence === operation.current;
        setBusy(true);
        setError("");
        setMessage("");
        try {
            await work(isCurrent);
        } catch (cause) {
            if (isCurrent()) {
                if (isSessionProblem(cause)) {
                    mounted.current = false;
                    onAccessLost();
                } else setError(describeError(cause));
            }
        } finally {
            if (sequence === operation.current) {
                if (isCurrent()) setBusy(false);
                operation.current += 1;
                operationPending.current = false;
            }
        }
    };

    const configure = (enabled: boolean, intervalMinutes: number) => {
        void run(async (isCurrent) => {
            const next = await api.configureSmartProxy(enabled, intervalMinutes);
            if (isCurrent()) {
                setStatus(next);
                setLoadError("");
            }
        });
    };

    const probe = () => {
        void run(async (isCurrent) => {
            await api.probeSmartProxy();
            if (!isCurrent()) return;
            setStatus((previous) =>
                previous ? { ...previous, running: true } : previous,
            );
            setMessage(messages().smart.probeRequested);
        });
    };

    /**
     * Fetches the credentials only on demand and never keeps them in state.
     * `pick` returns nothing when the server no longer offers the protocol,
     * for example after another browser disabled it.
     */
    const copyDetail = (
        pick: (details: SmartProxyDetails) => string | undefined,
        protocol: string,
        copiedMessage: string,
    ) => {
        void run(async (isCurrent) => {
            const details = await api.smartProxyDetails();
            if (!isCurrent()) return;
            const value = pick(details);
            if (!value) {
                setError(messages().smart.profileUnavailable(protocolName(protocol)));
                return;
            }
            const copied = await copyText(value, isCurrent);
            if (isCurrent())
                setMessage(copied ? copiedMessage : messages().smart.copyFailed);
        });
    };

    const copyAddress = () =>
        copyDetail((details) => details.url, "socks5", messages().smart.socksCopied);

    const copyShadowsocks = () =>
        copyDetail(
            (details) => profileFor(details, "shadowsocks")?.uri,
            "shadowsocks",
            messages().smart.shadowsocksCopied,
        );

    /** Copies the automatic route's Surge line for one enabled protocol. */
    const copySurge = (protocol: string) =>
        copyDetail(
            (details) => profileFor(details, protocol)?.surge,
            protocol,
            messages().smart.surgeCopied(protocolName(protocol)),
        );

    return {
        busy,
        configure,
        copyAddress,
        copyShadowsocks,
        copySurge,
        error,
        loadError,
        message,
        probe,
        status,
    };
}

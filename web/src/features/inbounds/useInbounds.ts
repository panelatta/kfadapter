import { useEffect, useMemo, useRef, useState } from "react";
import type { ApiClient } from "@/api";
import type { InboundChoices, InboundsStatus } from "@/types";
import { describeError, isSessionProblem } from "@/lib/errors";

/** The inbound protocols in console order; SOCKS5 is always enabled. */
export const inboundProtocols = ["socks5", "snell", "shadowsocks", "socks5-tls"] as const;

export function choicesFrom(status: InboundsStatus | null): InboundChoices {
    const enabled = (protocol: string) =>
        status?.inbounds.some(
            (inbound) => inbound.protocol === protocol && inbound.enabled,
        ) ?? false;
    return {
        snell: enabled("snell"),
        shadowsocks: enabled("shadowsocks"),
        socksTLS: enabled("socks5-tls"),
    };
}

/**
 * Loads the inbound protocol status and applies one change at a time.
 * Responses are dropped once the browser session changes or a newer request
 * started, so a stale answer never overwrites the latest choice.
 */
export function useInbounds({
    api,
    getSessionRevision,
    onAccessLost,
}: {
    api: ApiClient;
    getSessionRevision: () => number;
    onAccessLost: () => void;
}) {
    const [status, setStatus] = useState<InboundsStatus | null>(null);
    const [error, setError] = useState("");
    const [loadError, setLoadError] = useState("");
    const [busy, setBusy] = useState(false);
    const operation = useRef(0);
    const pending = useRef(false);
    const mounted = useRef(false);

    useEffect(() => {
        mounted.current = true;
        const revision = getSessionRevision();
        const sequence = ++operation.current;
        const isCurrent = () =>
            mounted.current &&
            revision === getSessionRevision() &&
            sequence === operation.current;
        api.inbounds()
            .then((next) => {
                if (!isCurrent()) return;
                setStatus(next);
                setLoadError("");
            })
            .catch((cause: unknown) => {
                if (!isCurrent()) return;
                if (isSessionProblem(cause)) {
                    mounted.current = false;
                    onAccessLost();
                } else setLoadError(describeError(cause));
            });
        return () => {
            mounted.current = false;
            operation.current += 1;
            pending.current = false;
        };
    }, [api, getSessionRevision, onAccessLost]);

    const configure = (choices: InboundChoices) => {
        if (!mounted.current || pending.current) return;
        const revision = getSessionRevision();
        const sequence = ++operation.current;
        const isCurrent = () =>
            mounted.current &&
            revision === getSessionRevision() &&
            sequence === operation.current;
        pending.current = true;
        setBusy(true);
        setError("");
        api.configureInbounds(choices)
            .then((next) => {
                if (!isCurrent()) return;
                setStatus(next);
                setLoadError("");
            })
            .catch((cause: unknown) => {
                if (!isCurrent()) return;
                if (isSessionProblem(cause)) {
                    mounted.current = false;
                    onAccessLost();
                } else setError(describeError(cause));
            })
            .finally(() => {
                if (sequence !== operation.current) return;
                pending.current = false;
                if (isCurrent()) setBusy(false);
            });
    };

    const enabledProtocols = useMemo(
        () =>
            inboundProtocols.filter(
                (protocol) =>
                    protocol === "socks5" ||
                    status?.inbounds.some(
                        (inbound) =>
                            inbound.protocol === protocol && inbound.enabled,
                    ),
            ),
        [status],
    );

    return { busy, configure, enabledProtocols, error, loadError, status };
}

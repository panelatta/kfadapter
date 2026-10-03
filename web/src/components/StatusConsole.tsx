import { useMemo, useState, type Dispatch, type SetStateAction } from "react";
import { Copy, Download, LockKeyhole, LogOut, RefreshCw } from "lucide-react";
import { ApiClient } from "@/api";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardAction, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { providerLabel, stateDetails, groupNameCollator, compareGroupNames, type StatusDetail } from "@/lib/console";
import { isSessionProblem, describeError } from "@/lib/errors";
import { formatTime, formatCompactTime, describeSubscriptionValidity } from "@/lib/format";
import { useCopyFeedback } from "@/hooks/useCopyFeedback";
import { selectClassName, ProviderCredentialsForm, Field, FormError } from "@/components/AuthCards";
import { SmartProxyCard } from "@/components/SmartProxyCard";
import { NodeList } from "@/components/NodeList";
import type { NodeRecord, StatusResponse } from "@/types";

export function StatusConsole({
    api,
    getSessionRevision,
    loadError,
    nodes,
    onAccessLost,
    onLockConsole,
    onLoginAccount,
    onNodesChange,
    onRefreshAll,
    onRefreshProvider,
    onRemoveProvider,
    onSignOutAll,
    refreshing,
    status,
    subscriptionURL,
}: {
    api: ApiClient;
    getSessionRevision: () => number;
    loadError: string;
    nodes: NodeRecord[];
    onAccessLost: () => void;
    onLockConsole: () => Promise<void>;
    onLoginAccount: (
        provider: string,
        account: string,
        password: string,
    ) => Promise<void>;
    onNodesChange: Dispatch<SetStateAction<NodeRecord[]>>;
    onRefreshAll: () => Promise<void>;
    onRefreshProvider: (provider: string) => Promise<void>;
    onRemoveProvider: (provider: string) => Promise<void>;
    onSignOutAll: () => Promise<void>;
    refreshing: boolean;
    status: StatusResponse;
    subscriptionURL: string;
}) {
    const detail = stateDetails[status.state];
    const [selectedProvider, setActiveProvider] = useState("all");
    const [filterProvider, setFilterProvider] = useState("");
    const [selectedGroup, setFilterGroup] = useState("");
    const [filterName, setFilterName] = useState("");
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

    const accounts = status.accounts ?? {};
    const eligibleNodes = useMemo(
        () => nodes.filter((node) => node.eligible),
        [nodes],
    );
    const visibleNodes =
        activeProvider === "all"
            ? eligibleNodes
            : eligibleNodes.filter((node) => node.provider === activeProvider);
    const nodeProviders = useMemo(
        () =>
            [...new Set(eligibleNodes.map((node) => node.provider))].sort(
                groupNameCollator.compare,
            ),
        [eligibleNodes],
    );
    const filterGroups = useMemo(
        () =>
            [
                ...new Set(
                    eligibleNodes
                        .filter(
                            (node) =>
                                !filterProvider ||
                                node.provider === filterProvider,
                        )
                        .map((node) => node.group),
                ),
            ].sort(compareGroupNames),
        [eligibleNodes, filterProvider],
    );

    // A group filter that no longer matches any node is ignored.
    const filterGroup = filterGroups.includes(selectedGroup)
        ? selectedGroup
        : "";

    const filteredSubscriptionURL = useMemo(() => {
        if (!subscriptionURL) return "";
        try {
            const next = new URL(subscriptionURL);
            if (filterProvider)
                next.searchParams.set("provider", filterProvider);
            if (filterGroup) next.searchParams.set("group", filterGroup);
            if (filterName.trim())
                next.searchParams.set("name", filterName.trim());
            return next.toString();
        } catch {
            return subscriptionURL;
        }
    }, [filterGroup, filterName, filterProvider, subscriptionURL]);

    const {
        message: copyMessage,
        copy: copySubscription,
        clear: clearCopyMessage,
    } = useCopyFeedback(filteredSubscriptionURL, "Link copied.");

    const selectProvider = (provider: string) => {
        setActiveProvider(provider);
        setProviderError("");
        setConfirmRemoval("");
        if (provider !== "all") {
            clearCopyMessage();
            setFilterProvider("");
            setFilterGroup("");
            setFilterName("");
        }
    };

    const [exportingDiagnostics, setExportingDiagnostics] = useState(false);
    const [diagnosticsError, setDiagnosticsError] = useState("");
    const exportDiagnostics = async () => {
        setExportingDiagnostics(true);
        setDiagnosticsError("");
        try {
            const report = await api.diagnostics();
            const url = URL.createObjectURL(report);
            const link = document.createElement("a");
            link.href = url;
            link.download = "kfadapter-diagnostics.json";
            link.click();
            URL.revokeObjectURL(url);
        } catch (error) {
            if (isSessionProblem(error)) onAccessLost();
            else setDiagnosticsError(describeError(error));
        } finally {
            setExportingDiagnostics(false);
        }
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

    const activeAccount =
        activeProvider === "all" ? undefined : accounts[activeProvider];
    const lastRefreshAt = formatTime(status.controlPlane.lastRefreshAt);
    const lastRefreshMetric = formatCompactTime(
        status.controlPlane.lastRefreshAt,
    );

    return (
        <main className="min-h-screen bg-background px-4 py-6 text-foreground sm:px-6 lg:px-10">
            <div className="boot mx-auto grid w-full max-w-6xl gap-6">
                <header className="border-b border-dashed border-border pb-5">
                    <div className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1">
                        <p className="glow font-sans text-sm font-bold uppercase tracking-[0.34em] text-primary">
                            Kfadapter
                        </p>
                        <p className="hidden text-[0.625rem] uppercase tracking-[0.24em] text-muted-foreground sm:block">
                            Local control plane · v{status.version}
                        </p>
                    </div>
                    <div className="mt-4 flex flex-col justify-between gap-4 sm:flex-row sm:items-end">
                        <div>
                            <div className="flex flex-nowrap items-center gap-2 sm:gap-3">
                                <h1 className="whitespace-nowrap font-sans text-xl font-semibold uppercase tracking-[0.04em] sm:text-2xl">
                                    Service status
                                </h1>
                                <StatusBadge detail={detail} />
                            </div>
                            {status.state === "ready" ? null : (
                                <p className="mt-2 text-sm text-muted-foreground">
                                    {detail.message}
                                </p>
                            )}
                        </div>
                        <div className="flex flex-nowrap items-center gap-1 sm:gap-2">
                            <Tooltip>
                                <TooltipTrigger asChild>
                                    <Button
                                        aria-label="Export diagnostics"
                                        disabled={exportingDiagnostics}
                                        onClick={() => void exportDiagnostics()}
                                        size="sm"
                                        variant="ghost"
                                    >
                                        <Download className="size-4" />
                                    </Button>
                                </TooltipTrigger>
                                <TooltipContent>
                                    Download a redacted diagnostics report
                                </TooltipContent>
                            </Tooltip>
                            <Button
                                aria-label="Lock console"
                                onClick={() => void onLockConsole()}
                                size="sm"
                                variant="outline"
                            >
                                <LockKeyhole className="size-4" />
                                <span aria-hidden="true" className="sm:hidden">
                                    Lock
                                </span>
                                <span
                                    aria-hidden="true"
                                    className="hidden sm:inline"
                                >
                                    Lock console
                                </span>
                            </Button>
                        </div>
                    </div>
                </header>

                <nav
                    aria-label="Console views"
                    className="flex gap-2 overflow-x-auto border-b border-border pb-3"
                >
                    <Button
                        aria-pressed={activeProvider === "all"}
                        onClick={() => selectProvider("all")}
                        size="sm"
                        variant={
                            activeProvider === "all" ? "default" : "outline"
                        }
                    >
                        Status
                    </Button>
                    {status.providers.map((provider) => (
                        <Button
                            aria-pressed={activeProvider === provider}
                            className="shrink-0"
                            key={provider}
                            onClick={() => selectProvider(provider)}
                            size="sm"
                            variant={
                                activeProvider === provider
                                    ? "default"
                                    : "outline"
                            }
                        >
                            <span
                                aria-hidden="true"
                                className={`led ${accounts[provider] ? "led--ok" : ""}`}
                            />
                            {providerLabel(provider)}
                        </Button>
                    ))}
                </nav>

                {loadError ? <FormError message={loadError} /> : null}
                {diagnosticsError ? (
                    <FormError message={diagnosticsError} />
                ) : null}

                {activeProvider === "all" ? (
                    <Card className="panel">
                        <CardHeader>
                            <CardTitle>Providers</CardTitle>
                            <CardDescription>
                                {Object.keys(accounts).length} of{" "}
                                {status.providers.length} providers connected.
                            </CardDescription>
                        </CardHeader>
                        <CardContent className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
                            {status.providers.map((provider) => {
                                const account = accounts[provider];
                                const providerNodes = eligibleNodes.filter(
                                    (node) => node.provider === provider,
                                );
                                return (
                                    <button
                                        className="grid gap-2 rounded-sm border border-border bg-background/40 p-4 text-left transition-colors hover:border-primary/60 hover:bg-primary/[0.04]"
                                        key={provider}
                                        onClick={() => selectProvider(provider)}
                                        type="button"
                                    >
                                        <span className="flex items-center justify-between gap-3">
                                            <strong className="font-sans uppercase tracking-[0.06em]">
                                                {providerLabel(provider)}
                                            </strong>
                                            <span
                                                aria-hidden="true"
                                                className={`led ${account ? "led--ok" : ""}`}
                                            />
                                        </span>
                                        <span className="text-xs text-muted-foreground">
                                            {account
                                                ? account.display
                                                : "No account connected"}
                                        </span>
                                        <span className="font-sans text-sm font-semibold uppercase tracking-[0.08em] text-primary/90">
                                            {account
                                                ? account.tier
                                                : "Not connected"}
                                        </span>
                                        <span className="text-[0.625rem] uppercase tracking-[0.18em] text-muted-foreground">
                                            {providerNodes.length} eligible
                                            nodes
                                        </span>
                                    </button>
                                );
                            })}
                        </CardContent>
                        <CardFooter className="flex-wrap justify-between gap-3 border-t border-dashed border-border/70 pt-4 [&.border-t]:pt-4">
                            <p className="min-w-0 text-xs leading-5 text-muted-foreground">
                                Manage your linked accounts.
                            </p>
                            <div className="flex w-full flex-wrap gap-2 sm:ml-auto sm:w-auto sm:justify-end">
                                <Button
                                    disabled={refreshing || providerBusy}
                                    onClick={() => void onRefreshAll()}
                                    size="sm"
                                    variant="outline"
                                >
                                    <RefreshCw
                                        className={
                                            refreshing
                                                ? "size-4 animate-spin"
                                                : "size-4"
                                        }
                                    />
                                    Refresh all
                                </Button>
                                {confirmSignOutAll ? (
                                    <>
                                        <Button
                                            onClick={() =>
                                                setConfirmSignOutAll(false)
                                            }
                                            size="sm"
                                            variant="ghost"
                                        >
                                            Cancel
                                        </Button>
                                        <Button
                                            disabled={refreshing || providerBusy}
                                            onClick={() => {
                                                setConfirmSignOutAll(false);
                                                void onSignOutAll();
                                            }}
                                            size="sm"
                                            variant="destructive"
                                        >
                                            <LogOut />
                                            Confirm sign out
                                        </Button>
                                    </>
                                ) : (
                                    <Button
                                        className="border border-destructive/60 text-destructive hover:border-destructive hover:bg-destructive/10 hover:text-destructive"
                                        disabled={
                                            refreshing ||
                                            providerBusy ||
                                            Object.keys(accounts).length === 0
                                        }
                                        onClick={() =>
                                            setConfirmSignOutAll(true)
                                        }
                                        size="sm"
                                        variant="outline"
                                    >
                                        <LogOut />
                                        Sign out all
                                    </Button>
                                )}
                            </div>
                        </CardFooter>
                    </Card>
                ) : activeAccount ? (
                    <Card className="panel">
                        <CardHeader>
                            <CardTitle>
                                {providerLabel(activeProvider)} account
                            </CardTitle>
                            <CardDescription>
                                {activeAccount.display}
                            </CardDescription>
                        </CardHeader>
                        <CardContent className="grid gap-5 sm:grid-cols-2 xl:grid-cols-4">
                            <Metric
                                label="Subscription tier"
                                value={activeAccount.tier}
                            />
                            <SubscriptionMetric
                                subscriptionActive={
                                    activeAccount.subscriptionActive
                                }
                                subscriptionEndsAt={
                                    activeAccount.subscriptionEndsAt
                                }
                            />
                            <Metric
                                label="Eligible nodes"
                                value={String(visibleNodes.length)}
                            />
                            <Metric
                                label="Last refresh"
                                title={lastRefreshAt}
                                value={lastRefreshMetric}
                            />
                        </CardContent>
                        <CardFooter className="flex-wrap justify-between gap-3 border-t border-dashed border-border/70 pt-4 [&.border-t]:pt-4">
                            <FormError message={providerError} />
                            <div className="ml-auto flex flex-wrap justify-end gap-2">
                                <Button
                                    disabled={providerBusy || refreshing}
                                    onClick={() =>
                                        void runProviderAction(() =>
                                            onRefreshProvider(activeProvider),
                                        )
                                    }
                                    size="sm"
                                    variant="outline"
                                >
                                    <RefreshCw
                                        className={
                                            providerBusy
                                                ? "size-4 animate-spin"
                                                : "size-4"
                                        }
                                    />
                                    Refresh provider
                                </Button>
                                {confirmRemoval === activeProvider ? (
                                    <>
                                        <Button
                                            onClick={() =>
                                                setConfirmRemoval("")
                                            }
                                            size="sm"
                                            variant="ghost"
                                        >
                                            Cancel
                                        </Button>
                                        <Button
                                            disabled={providerBusy}
                                            onClick={() =>
                                                void runProviderAction(() =>
                                                    onRemoveProvider(
                                                        activeProvider,
                                                    ),
                                                )
                                            }
                                            size="sm"
                                            variant="destructive"
                                        >
                                            Confirm removal
                                        </Button>
                                    </>
                                ) : (
                                    <Button
                                        disabled={providerBusy || refreshing}
                                        onClick={() =>
                                            setConfirmRemoval(activeProvider)
                                        }
                                        size="sm"
                                        variant="outline"
                                    >
                                        Remove account
                                    </Button>
                                )}
                            </div>
                        </CardFooter>
                    </Card>
                ) : (
                    <Card className="panel max-w-xl">
                        <CardHeader>
                            <CardTitle>
                                Connect {providerLabel(activeProvider)}
                            </CardTitle>
                        </CardHeader>
                        <CardContent>
                            <ProviderCredentialsForm
                                fixedProvider={activeProvider}
                                idPrefix={`add-${activeProvider}`}
                                onSubmit={onLoginAccount}
                            />
                        </CardContent>
                    </Card>
                )}

                {activeProvider === "all" ? (
                    <section className="grid gap-4 md:grid-cols-[0.9fr_1.5fr]">
                        <Card className="panel">
                            <CardHeader>
                                <CardTitle>Service</CardTitle>
                            </CardHeader>
                            <CardContent className="grid gap-5 sm:grid-cols-2 md:grid-cols-1 lg:grid-cols-2">
                                <Metric
                                    label="Accounts"
                                    value={`${Object.keys(accounts).length} / ${status.providers.length}`}
                                />
                                <Metric
                                    label="Available nodes"
                                    value={`${status.nodes.eligible} / ${status.nodes.total}`}
                                />
                            </CardContent>
                            <CardFooter className="gap-2 text-xs text-muted-foreground">
                                <span
                                    aria-hidden="true"
                                    className={`led ${refreshing ? "led--warn led--pulse" : "led--ok"}`}
                                />
                                <span
                                    className="min-w-0 truncate"
                                    title={`Updated ${formatTime(status.controlPlane.lastRefreshAt)}`}
                                >
                                    Updated{" "}
                                    {formatTime(
                                        status.controlPlane.lastRefreshAt,
                                    )}
                                </span>
                            </CardFooter>
                        </Card>

                        <Card className="panel">
                            <CardHeader>
                                <CardTitle>Subscription link</CardTitle>
                                <CardAction>
                                    <Tooltip>
                                        <TooltipTrigger asChild>
                                            <Button
                                                aria-label="Copy subscription link"
                                                disabled={
                                                    !filteredSubscriptionURL
                                                }
                                                onClick={() =>
                                                    void copySubscription()
                                                }
                                                size="icon"
                                                variant="ghost"
                                            >
                                                <Copy className="size-4" />
                                            </Button>
                                        </TooltipTrigger>
                                        <TooltipContent>
                                            Copy filtered link
                                        </TooltipContent>
                                    </Tooltip>
                                </CardAction>
                            </CardHeader>
                            <CardContent className="grid gap-3">
                                <div className="grid gap-3 sm:grid-cols-3">
                                    <Field
                                        label="Provider"
                                        name="subscription-provider"
                                    >
                                        <select
                                            className={selectClassName}
                                            id="subscription-provider"
                                            onChange={(event) =>
                                                setFilterProvider(
                                                    event.currentTarget.value,
                                                )
                                            }
                                            value={filterProvider}
                                        >
                                            <option value="">
                                                All providers
                                            </option>
                                            {nodeProviders.map((provider) => (
                                                <option
                                                    key={provider}
                                                    value={provider}
                                                >
                                                    {providerLabel(provider)}
                                                </option>
                                            ))}
                                        </select>
                                    </Field>
                                    <Field
                                        label="Group"
                                        name="subscription-group"
                                    >
                                        <select
                                            className={selectClassName}
                                            id="subscription-group"
                                            onChange={(event) =>
                                                setFilterGroup(
                                                    event.currentTarget.value,
                                                )
                                            }
                                            value={filterGroup}
                                        >
                                            <option value="">All groups</option>
                                            {filterGroups.map((group) => (
                                                <option
                                                    key={group}
                                                    value={group}
                                                >
                                                    {group}
                                                </option>
                                            ))}
                                        </select>
                                    </Field>
                                    <Field
                                        label="Node name"
                                        name="subscription-name"
                                    >
                                        <Input
                                            id="subscription-name"
                                            onChange={(event) =>
                                                setFilterName(
                                                    event.currentTarget.value,
                                                )
                                            }
                                            placeholder="Contains…"
                                            value={filterName}
                                        />
                                    </Field>
                                </div>
                                <code
                                    aria-busy={!filteredSubscriptionURL}
                                    className="readout block break-all rounded-sm border border-border/70 px-3 py-2.5 text-xs leading-5 text-primary/90"
                                >
                                    {filteredSubscriptionURL || (
                                        <span className="animate-pulse text-muted-foreground">
                                            Loading link…
                                        </span>
                                    )}
                                </code>
                                <p
                                    aria-live="polite"
                                    className="min-h-5 text-xs text-muted-foreground"
                                    role="status"
                                >
                                    {copyMessage}
                                </p>
                            </CardContent>
                        </Card>
                    </section>
                ) : null}

                <SmartProxyCard api={api} getSessionRevision={getSessionRevision} onAccessLost={onAccessLost} />

                {activeAccount ? (
                    <NodeList
                        api={api}
                        getSessionRevision={getSessionRevision}
                        nodes={visibleNodes}
                        onAccessLost={onAccessLost}
                        onNodesChange={onNodesChange}
                    />
                ) : null}
            </div>
        </main>
    );
}

function StatusBadge({ detail }: { detail: StatusDetail }) {
    const tone = {
        ready: {
            badge: "border-led-ok/35 bg-led-ok/10 text-led-ok",
            led: "led--ok",
        },
        working: {
            badge: "border-led-warn/35 bg-led-warn/10 text-led-warn",
            led: "led--warn led--pulse",
        },
        warning: {
            badge: "border-led-warn/35 bg-led-warn/10 text-led-warn",
            led: "led--warn",
        },
        danger: {
            badge: "border-destructive/35 bg-destructive/10 text-destructive",
            led: "led--err",
        },
        neutral: {
            badge: "border-border bg-muted text-muted-foreground",
            led: "",
        },
    }[detail.tone];
    return (
        <Badge className={`h-8 gap-2 px-2.5 ${tone.badge}`} variant="outline">
            <span aria-hidden="true" className={`led ${tone.led}`} />
            {detail.label}
        </Badge>
    );
}

function Metric({
    label,
    title,
    value,
}: {
    label: string;
    title?: string;
    value: string;
}) {
    return (
        <div className="min-w-0 border-l border-primary/40 pl-3">
            <p className="text-[0.65rem] font-semibold uppercase tracking-[0.2em] text-muted-foreground">
                {label}
            </p>
            <p
                className="mt-1.5 break-words font-sans text-2xl font-semibold leading-tight tabular-nums"
                title={title}
            >
                {value}
            </p>
        </div>
    );
}

function SubscriptionMetric({
    subscriptionActive,
    subscriptionEndsAt,
}: {
    subscriptionActive: boolean;
    subscriptionEndsAt?: string;
}) {
    const validity = describeSubscriptionValidity(
        subscriptionActive,
        subscriptionEndsAt,
    );
    return (
        <div className="border-l border-primary/40 pl-3">
            <p className="text-[0.65rem] font-semibold uppercase tracking-[0.2em] text-muted-foreground">
                Subscription
            </p>
            <p className="mt-1.5 break-words font-sans text-lg font-semibold leading-tight tabular-nums">
                {validity.summary}
            </p>
            {validity.endsAt ? (
                <p
                    className="mt-1 text-xs text-muted-foreground"
                    title={validity.endsAt}
                >
                    Ends {validity.endsAt}
                </p>
            ) : null}
        </div>
    );
}

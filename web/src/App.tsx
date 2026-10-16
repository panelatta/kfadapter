import { useMemo, useState } from "react";
import type { Dispatch, ReactNode, SetStateAction } from "react";
import { Download, KeyRound, LayoutGrid, LockKeyhole } from "lucide-react";
import type { ApiClient } from "./api";
import type { NodeRecord, StatusResponse } from "./types";
import { BrandMark } from "@/components/BrandMark";
import { FormError } from "@/components/forms";
import { LanguageSwitch } from "@/components/LanguageSwitch";
import { Button } from "@/components/ui/button";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";
import { AccessTokenCard } from "@/features/access/AccessTokenCard";
import { ChangeAccessTokenCard } from "@/features/access/ChangeAccessTokenCard";
import { LoadingCard } from "@/features/access/LoadingCard";
import { ServiceErrorCard } from "@/features/access/ServiceErrorCard";
import { useConsoleSession } from "@/features/access/useConsoleSession";
import { AccountPanel } from "@/features/accounts/AccountPanel";
import { accountIssue, accountTone, lampClass } from "@/features/accounts/accountHealth";
import { ProviderLoginCard } from "@/features/accounts/ProviderLoginCard";
import { providerLabel, providerLoginStates } from "@/features/accounts/providers";
import { StatusBadge, stateDetail } from "@/features/accounts/StatusBadge";
import { useAccountViews } from "@/features/accounts/useAccountViews";
import { useDiagnosticsExport } from "@/features/diagnostics/useDiagnosticsExport";
import { NodeList } from "@/features/nodes/NodeList";
import { SmartProxyCard } from "@/features/smart/SmartProxyCard";
import { SubscriptionLinkCard } from "@/features/subscription/SubscriptionLinkCard";
import { useSubscriptionLink } from "@/features/subscription/useSubscriptionLink";
import { useI18n } from "@/i18n";
import { cn } from "@/lib/utils";

interface AppProps {
    api?: ApiClient;
}

export function App({ api: providedApi }: AppProps) {
    const {
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
        loginAccount,
        changeAccessToken,
        loseAccessSession,
        lockConsole,
        retryLoad,
        getSessionRevision,
        setNodes,
        refreshAll,
        refreshProvider,
        removeProvider,
        signOutAll,
    } = useConsoleSession(providedApi);
    // Subscribing here re-renders the whole console when the language changes.
    const { m } = useI18n();

    let content;
    if (session === "checking") {
        content = <LoadingCard />;
    } else if (session === "locking") {
        content = (
            <LoadingCard
                title={m.access.lockingTitle}
                description={m.access.lockingDescription}
            />
        );
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
        // The status is still loading or failed to load. Neither case means the
        // provider account is signed out.
        content = loadError ? (
            <ServiceErrorCard message={loadError} onRetry={retryLoad} />
        ) : (
            <LoadingCard description={m.access.loadingStatus} />
        );
    } else if (route === "/signin" || providerLoginStates[status.state]) {
        content = (
            <ProviderLoginCard
                providers={status.providers}
                providerAuth={status.providerAuth}
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
                onChangeAccessToken={changeAccessToken}
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

function StatusConsole({
    api,
    getSessionRevision,
    loadError,
    nodes,
    onAccessLost,
    onChangeAccessToken,
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
    onChangeAccessToken: (current: string, next: string) => Promise<void>;
    onLockConsole: () => Promise<void>;
    onLoginAccount: (
        provider: string,
        fields: Record<string, string>,
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
    const { m } = useI18n();
    const detail = stateDetail(status.state);
    const accounts = status.accounts ?? {};
    const eligibleNodes = useMemo(
        () => nodes.filter((node) => node.eligible),
        [nodes],
    );
    const subscriptionLink = useSubscriptionLink(eligibleNodes, subscriptionURL);
    const accountView = useAccountViews(status, subscriptionLink.resetFilters);
    const { activeProvider, selectProvider } = accountView;
    const diagnostics = useDiagnosticsExport({
        api,
        getSessionRevision,
        onAccessLost,
    });
    const visibleNodes =
        activeProvider === "all"
            ? eligibleNodes
            : eligibleNodes.filter((node) => node.provider === activeProvider);
    const activeAccount =
        activeProvider === "all" ? undefined : accounts[activeProvider];
    const [changingToken, setChangingToken] = useState(false);
    const [tokenChanged, setTokenChanged] = useState(false);
    const changeToken = async (current: string, next: string) => {
        await onChangeAccessToken(current, next);
        setChangingToken(false);
        setTokenChanged(true);
    };
    const issues = status.providers.flatMap((provider) => {
        const issue = accountIssue(accounts[provider]);
        return issue ? [{ provider, ...issue }] : [];
    });

    return (
        <div className="min-h-screen bg-background text-foreground lg:grid lg:grid-cols-[15rem_minmax(0,1fr)]">
            <aside className="border-b border-border bg-card lg:sticky lg:top-0 lg:flex lg:h-screen lg:flex-col lg:border-b-0 lg:border-r">
                <div className="flex items-center justify-between gap-3 px-4 pb-3 pt-4 sm:px-6 lg:px-5 lg:pb-6 lg:pt-6">
                    <BrandMark version={m.console.version(status.version)} />
                    <LanguageSwitch className="lg:hidden" />
                </div>
                <nav
                    aria-label={m.console.views}
                    className="flex gap-1 overflow-x-auto px-4 pb-3 [scrollbar-width:none] sm:px-6 lg:flex-col lg:overflow-visible lg:px-3 lg:pb-0 [&::-webkit-scrollbar]:hidden"
                >
                    <ViewTab
                        active={activeProvider === "all"}
                        onClick={() => selectProvider("all")}
                    >
                        <LayoutGrid aria-hidden="true" className="size-4" />
                        {m.console.overview}
                    </ViewTab>
                    <span
                        aria-hidden="true"
                        className="mx-1 my-1.5 w-px shrink-0 bg-border lg:mx-2 lg:my-2 lg:h-px lg:w-auto"
                    />
                    {status.providers.map((provider) => (
                        <ViewTab
                            active={activeProvider === provider}
                            key={provider}
                            onClick={() => selectProvider(provider)}
                        >
                            <span
                                aria-hidden="true"
                                className={`led ${lampClass[accountTone(accounts[provider])]}`}
                            />
                            {providerLabel(provider)}
                        </ViewTab>
                    ))}
                </nav>
                <div className="mt-auto hidden px-5 py-5 lg:block">
                    <LanguageSwitch />
                </div>
            </aside>

            <main className="min-w-0 px-4 py-6 sm:px-6 lg:px-10 lg:py-10">
                <div className="mx-auto grid w-full max-w-5xl grid-cols-[minmax(0,1fr)] gap-5">
                    <header className="mb-1 flex flex-col justify-between gap-4 sm:flex-row sm:items-start">
                        <div className="min-w-0">
                            <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
                                <h1 className="text-2xl font-semibold tracking-[-0.015em] sm:text-[1.75rem]">
                                    {m.console.title}
                                </h1>
                                <StatusBadge state={status.state} />
                            </div>
                            {issues.length > 0 ? (
                                <ul
                                    aria-label={m.console.problems}
                                    className="mt-3 grid gap-1.5"
                                >
                                    {issues.map((issue) => (
                                        <li key={issue.provider}>
                                            <button
                                                className="group flex min-w-0 items-baseline gap-2 text-left text-sm"
                                                onClick={() =>
                                                    selectProvider(issue.provider)
                                                }
                                                type="button"
                                            >
                                                <span
                                                    aria-hidden="true"
                                                    className={`led ${lampClass[issue.tone]} relative -top-px`}
                                                />
                                                <span className="font-medium underline-offset-4 group-hover:underline">
                                                    {providerLabel(issue.provider)}
                                                </span>
                                                <span className="text-muted-foreground">
                                                    {m.health.sentence(
                                                        issue.summary,
                                                        issue.action,
                                                    )}
                                                </span>
                                            </button>
                                        </li>
                                    ))}
                                </ul>
                            ) : status.state === "ready" ? null : (
                                <p className="mt-2 text-sm text-muted-foreground">
                                    {detail.message}
                                </p>
                            )}
                        </div>
                        <div className="flex shrink-0 flex-nowrap items-center gap-1 self-start">
                            <Tooltip>
                                <TooltipTrigger asChild>
                                    <Button
                                        aria-label={m.console.exportDiagnostics}
                                        disabled={diagnostics.exporting}
                                        onClick={() =>
                                            void diagnostics.exportDiagnostics()
                                        }
                                        size="icon"
                                        variant="ghost"
                                    >
                                        <Download className="size-4" />
                                    </Button>
                                </TooltipTrigger>
                                <TooltipContent>
                                    {m.console.exportDiagnosticsHint}
                                </TooltipContent>
                            </Tooltip>
                            <Tooltip>
                                <TooltipTrigger asChild>
                                    <Button
                                        aria-expanded={changingToken}
                                        aria-label={m.console.changeToken}
                                        onClick={() => {
                                            setChangingToken((open) => !open);
                                            setTokenChanged(false);
                                        }}
                                        size="icon"
                                        variant="ghost"
                                    >
                                        <KeyRound className="size-4" />
                                    </Button>
                                </TooltipTrigger>
                                <TooltipContent>{m.console.changeToken}</TooltipContent>
                            </Tooltip>
                            <Button
                                aria-label={m.console.lockConsole}
                                className="ml-1"
                                onClick={() => void onLockConsole()}
                                size="sm"
                                variant="outline"
                            >
                                <LockKeyhole className="size-4" />
                                <span aria-hidden="true">{m.console.lock}</span>
                            </Button>
                        </div>
                    </header>

                    {changingToken ? (
                        <ChangeAccessTokenCard
                            onCancel={() => setChangingToken(false)}
                            onSubmit={changeToken}
                        />
                    ) : null}
                    {tokenChanged ? (
                        <p
                            className="rounded-md border border-led-ok/25 bg-success px-3.5 py-2.5 text-sm text-success-foreground"
                            role="status"
                        >
                            {m.changeToken.done}
                        </p>
                    ) : null}

                    {loadError ? <FormError message={loadError} /> : null}
                    {diagnostics.error ? (
                        <FormError message={diagnostics.error} />
                    ) : null}

                    <AccountPanel
                        accountView={accountView}
                        eligibleNodes={eligibleNodes}
                        onLoginAccount={onLoginAccount}
                        onRefreshAll={onRefreshAll}
                        onRefreshProvider={onRefreshProvider}
                        onRemoveProvider={onRemoveProvider}
                        onSignOutAll={onSignOutAll}
                        refreshing={refreshing}
                        status={status}
                    />

                    {activeProvider === "all" ? (
                        <SubscriptionLinkCard link={subscriptionLink} />
                    ) : null}

                    <SmartProxyCard
                        api={api}
                        getSessionRevision={getSessionRevision}
                        onAccessLost={onAccessLost}
                    />

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
        </div>
    );
}

// The active view is tinted rather than filled so its status dot stays visible.
function ViewTab({
    active,
    children,
    onClick,
}: {
    active: boolean;
    children: ReactNode;
    onClick: () => void;
}) {
    return (
        <button
            aria-pressed={active}
            className={cn(
                "flex h-9 shrink-0 items-center gap-2.5 rounded-md px-3 text-sm font-medium text-muted-foreground transition-colors hover:bg-muted hover:text-foreground lg:w-full",
                active && "bg-accent text-accent-foreground hover:bg-accent hover:text-accent-foreground",
            )}
            onClick={onClick}
            type="button"
        >
            {children}
        </button>
    );
}

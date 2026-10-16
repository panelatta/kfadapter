import { ChevronRight, LogOut, RefreshCw } from "lucide-react";
import type { AccountSummary, NodeRecord, StatusResponse } from "@/types";
import { FormError } from "@/components/forms";
import { Metric } from "@/components/Metric";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from "@/components/ui/card";
import { useI18n } from "@/i18n";
import { formatCompactTime, formatTime } from "@/lib/time";
import { accountIssue, accountTone, lampClass } from "./accountHealth";
import type { AccountIssue } from "./accountHealth";
import { ProviderCredentialsForm } from "./ProviderCredentialsForm";
import { SubscriptionMetric } from "./SubscriptionMetric";
import { providerLabel } from "./providers";
import type { AccountViews } from "./useAccountViews";

const removeAllKey = "\u0000all";
const footerClassName =
    "flex-wrap justify-between gap-3 border-t border-border pt-4 [&.border-t]:pt-4";

export function AccountPanel({
    accountView,
    eligibleNodes,
    onLoginAccount,
    onRefreshAll,
    onRefreshProvider,
    onRemoveProvider,
    onSignOutAll,
    refreshing,
    status,
}: {
    accountView: AccountViews;
    eligibleNodes: NodeRecord[];
    onLoginAccount: (provider: string, fields: Record<string, string>) => Promise<void>;
    onRefreshAll: () => Promise<void>;
    onRefreshProvider: (provider: string) => Promise<void>;
    onRemoveProvider: (provider: string) => Promise<void>;
    onSignOutAll: () => Promise<void>;
    refreshing: boolean;
    status: StatusResponse;
}) {
    const { m } = useI18n();
    const {
        activeProvider,
        confirmRemoval,
        providerBusy,
        providerError,
        runProviderAction,
        selectProvider,
        setConfirmRemoval,
    } = accountView;
    const accounts = status.accounts ?? {};
    const linkedCount = Object.keys(accounts).length;
    // A public provider (no credential fields) has no account identity to show.
    const isPublic = (provider: string) =>
        status.providerAuth?.[provider]?.fields.length === 0;

    if (activeProvider === "all") {
        return (
            <Card className="pb-4">
                <CardHeader>
                    <CardTitle>{m.providers.title}</CardTitle>
                </CardHeader>
                <CardContent className="px-0 sm:px-0">
                    <ul className="divide-y divide-border border-y border-border">
                        {status.providers.map((provider) => (
                            <li key={provider}>
                                <ProviderRow
                                    account={accounts[provider]}
                                    isPublic={isPublic(provider)}
                                    nodeCount={
                                        eligibleNodes.filter(
                                            (node) => node.provider === provider,
                                        ).length
                                    }
                                    onSelect={() => selectProvider(provider)}
                                    provider={provider}
                                />
                            </li>
                        ))}
                    </ul>
                </CardContent>
                <CardFooter className="flex-wrap justify-between gap-3">
                    <p
                        className="flex min-w-0 items-center gap-2 text-[0.8125rem] text-muted-foreground"
                        title={m.providers.updated(
                            formatTime(status.controlPlane.lastRefreshAt),
                        )}
                    >
                        <span
                            aria-hidden="true"
                            className={`led ${refreshing ? "led--warn led--pulse" : "led--ok"}`}
                        />
                        <span className="truncate">
                            {m.providers.usable(
                                status.nodes.eligible,
                                status.nodes.total,
                            )}{" "}
                            ·{" "}
                            {m.providers.updated(
                                formatCompactTime(
                                    status.controlPlane.lastRefreshAt,
                                ),
                            )}
                        </span>
                    </p>
                    <div className="flex w-full flex-wrap gap-2 sm:ml-auto sm:w-auto sm:justify-end">
                        {confirmRemoval === removeAllKey ? (
                            <>
                                <Button
                                    onClick={() => setConfirmRemoval("")}
                                    size="sm"
                                    variant="ghost"
                                >
                                    {m.providers.cancel}
                                </Button>
                                <Button
                                    disabled={refreshing || providerBusy}
                                    onClick={() =>
                                        void runProviderAction(onSignOutAll)
                                    }
                                    size="sm"
                                    variant="destructive"
                                >
                                    <LogOut />
                                    {m.providers.removeCount(linkedCount)}
                                </Button>
                            </>
                        ) : (
                            <>
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
                                    {m.providers.refreshAll}
                                </Button>
                                <Button
                                    className="text-destructive hover:bg-destructive/[0.06] hover:text-destructive"
                                    disabled={
                                        refreshing ||
                                        providerBusy ||
                                        linkedCount === 0
                                    }
                                    onClick={() =>
                                        setConfirmRemoval(removeAllKey)
                                    }
                                    size="sm"
                                    variant="ghost"
                                >
                                    <LogOut />
                                    {m.providers.removeAll}
                                </Button>
                            </>
                        )}
                    </div>
                </CardFooter>
                {providerError ? (
                    <div className="px-5 sm:px-6">
                        <FormError message={providerError} />
                    </div>
                ) : null}
            </Card>
        );
    }

    const activeAccount = accounts[activeProvider];
    if (!activeAccount) {
        return (
            <Card className="max-w-xl">
                <CardHeader>
                    <CardTitle>
                        {m.providers.add(providerLabel(activeProvider))}
                    </CardTitle>
                </CardHeader>
                <CardContent>
                    <ProviderCredentialsForm
                        fixedProvider={activeProvider}
                        idPrefix={`add-${activeProvider}`}
                        key={activeProvider}
                        onSubmit={onLoginAccount}
                        providerAuth={status.providerAuth}
                    />
                </CardContent>
            </Card>
        );
    }

    const publicProvider = isPublic(activeProvider);
    const issue = accountIssue(activeAccount);
    return (
        <Card>
            <CardHeader>
                <CardTitle className="text-lg">
                    {providerLabel(activeProvider)}
                </CardTitle>
                <CardDescription>
                    {publicProvider
                        ? m.providers.publicService
                        : activeAccount.display}
                </CardDescription>
            </CardHeader>
            <CardContent className="grid gap-5">
                {issue ? <IssueNotice issue={issue} /> : null}
                <div className="grid gap-x-6 gap-y-5 sm:grid-cols-2 xl:grid-cols-4">
                    {publicProvider ? null : (
                        <>
                            <Metric
                                label={m.providers.tier}
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
                        </>
                    )}
                    <Metric
                        label={m.providers.lastRefresh}
                        title={formatTime(activeAccount.lastRefreshAt)}
                        value={formatCompactTime(activeAccount.lastRefreshAt)}
                    />
                    <Metric
                        label={m.providers.nextRefresh}
                        title={formatTime(activeAccount.nextRefreshAt)}
                        value={
                            activeAccount.refreshStatus === "needs_relogin"
                                ? m.providers.paused
                                : formatCompactTime(activeAccount.nextRefreshAt)
                        }
                    />
                </div>
            </CardContent>
            <CardFooter className={footerClassName}>
                <FormError message={providerError} />
                <div className="ml-auto flex flex-wrap justify-end gap-2">
                    {confirmRemoval === activeProvider ? (
                        <>
                            <Button
                                onClick={() => setConfirmRemoval("")}
                                size="sm"
                                variant="ghost"
                            >
                                {m.providers.cancel}
                            </Button>
                            <Button
                                disabled={providerBusy}
                                onClick={() =>
                                    void runProviderAction(() =>
                                        onRemoveProvider(activeProvider),
                                    )
                                }
                                size="sm"
                                variant="destructive"
                            >
                                {m.providers.confirmRemoval}
                            </Button>
                        </>
                    ) : (
                        <>
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
                                {m.providers.refresh}
                            </Button>
                            <Button
                                className="text-destructive hover:bg-destructive/[0.06] hover:text-destructive"
                                disabled={providerBusy || refreshing}
                                onClick={() =>
                                    setConfirmRemoval(activeProvider)
                                }
                                size="sm"
                                variant="ghost"
                            >
                                {m.providers.remove}
                            </Button>
                        </>
                    )}
                </div>
            </CardFooter>
        </Card>
    );
}

function ProviderRow({
    account,
    isPublic,
    nodeCount,
    onSelect,
    provider,
}: {
    account?: AccountSummary;
    isPublic: boolean;
    nodeCount: number;
    onSelect: () => void;
    provider: string;
}) {
    const { m } = useI18n();
    const issue = accountIssue(account);
    const issueText =
        issue?.tone === "err" ? "text-destructive" : "text-warning-foreground";
    return (
        <button
            className="group flex w-full items-center gap-3 px-5 py-3.5 text-left transition-colors hover:bg-muted/70 sm:px-6"
            onClick={onSelect}
            type="button"
        >
            <span
                aria-hidden="true"
                className={`led ${lampClass[accountTone(account)]}`}
            />
            <span className="grid min-w-0 flex-1 gap-0.5 sm:grid-cols-[minmax(8rem,14rem)_minmax(0,1fr)] sm:items-baseline sm:gap-4">
                <strong
                    className={`truncate font-medium ${account ? "" : "text-muted-foreground"}`}
                >
                    {providerLabel(provider)}
                </strong>
                <span className="truncate text-[0.8125rem] text-muted-foreground">
                    {!account
                        ? m.providers.notAdded
                        : isPublic
                          ? m.providers.publicService
                          : (account.display ?? m.providers.accountConnected)}
                </span>
            </span>
            {account ? (
                issue ? (
                    <span
                        className={`max-w-[40%] truncate text-[0.8125rem] font-medium ${issueText}`}
                    >
                        {issue.summary}
                    </span>
                ) : (
                    <span className="shrink-0 text-[0.8125rem] tabular-nums text-muted-foreground">
                        {isPublic ? "" : `${account.tier} · `}
                        {m.providers.nodeCount(nodeCount)}
                    </span>
                )
            ) : null}
            <ChevronRight
                aria-hidden="true"
                className="size-4 shrink-0 text-muted-foreground/60 transition-transform group-hover:translate-x-0.5 group-hover:text-muted-foreground"
            />
        </button>
    );
}

function IssueNotice({ issue }: { issue: AccountIssue }) {
    const { m } = useI18n();
    const tone =
        issue.tone === "err"
            ? "border-destructive/25 bg-destructive/[0.06] text-destructive"
            : "border-led-warn/30 bg-warning text-warning-foreground";
    return (
        <p
            className={`flex items-start gap-2.5 rounded-md border px-3 py-2.5 text-sm leading-5 ${tone}`}
            role="status"
        >
            <span aria-hidden="true" className={`led ${lampClass[issue.tone]} mt-1.5`} />
            <span className="min-w-0">
                <strong className="font-semibold">
                    {m.health.summary(issue.summary)}
                </strong>
                {m.health.gap}
                <span className="opacity-85">{issue.action}</span>
            </span>
        </p>
    );
}

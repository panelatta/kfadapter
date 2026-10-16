import { Check, Copy } from "lucide-react";
import { Field, selectClassName } from "@/components/forms";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { providerLabel } from "@/features/accounts/providers";
import { useI18n } from "@/i18n";
import type { useSubscriptionLink } from "./useSubscriptionLink";

export function SubscriptionLinkCard({ link }: { link: ReturnType<typeof useSubscriptionLink> }) {
    const { m } = useI18n();
    const {
        copyMessage,
        copySubscription,
        filterGroup,
        filterGroups,
        filterName,
        filterProvider,
        filteredSubscriptionURL,
        nodeProviders,
        setFilterGroup,
        setFilterName,
        setFilterProvider,
    } = link;
    // Success shows on the button itself; only a failure needs a visible line.
    const copied = copyMessage === m.subscription.copied;

    return (
        <Card>
            <CardHeader>
                <CardTitle>{m.subscription.title}</CardTitle>
                <CardDescription>{m.subscription.description}</CardDescription>
            </CardHeader>
            <CardContent className="grid gap-4">
                <div className="grid gap-3 sm:grid-cols-3">
                    <Field
                        label={m.subscription.provider}
                        name="subscription-provider"
                    >
                        <select
                            className={selectClassName}
                            id="subscription-provider"
                            onChange={(event) =>
                                setFilterProvider(event.currentTarget.value)
                            }
                            value={filterProvider}
                        >
                            <option value="">
                                {m.subscription.allProviders}
                            </option>
                            {nodeProviders.map((provider) => (
                                <option key={provider} value={provider}>
                                    {providerLabel(provider)}
                                </option>
                            ))}
                        </select>
                    </Field>
                    <Field label={m.subscription.group} name="subscription-group">
                        <select
                            className={selectClassName}
                            id="subscription-group"
                            onChange={(event) =>
                                setFilterGroup(event.currentTarget.value)
                            }
                            value={filterGroup}
                        >
                            <option value="">{m.subscription.allGroups}</option>
                            {filterGroups.map((group) => (
                                <option key={group} value={group}>
                                    {group}
                                </option>
                            ))}
                        </select>
                    </Field>
                    <Field label={m.subscription.name} name="subscription-name">
                        <Input
                            id="subscription-name"
                            onChange={(event) =>
                                setFilterName(event.currentTarget.value)
                            }
                            placeholder={m.subscription.namePlaceholder}
                            value={filterName}
                        />
                    </Field>
                </div>
                <div className="flex flex-col rounded-lg bg-ticket text-ticket-foreground sm:flex-row">
                    <code
                        aria-busy={!filteredSubscriptionURL}
                        className="block min-w-0 flex-1 break-all px-4 py-3.5 font-mono text-[0.8125rem] leading-6"
                    >
                        {filteredSubscriptionURL || (
                            <span className="animate-pulse text-ticket-muted">
                                {m.subscription.loading}
                            </span>
                        )}
                    </code>
                    <div aria-hidden="true" className="ticket-divider" />
                    <div className="flex items-center p-3 sm:px-4">
                        <Button
                            aria-label={m.subscription.copy}
                            className="w-full bg-ticket-foreground text-ticket shadow-none hover:bg-ticket-foreground/90 focus-visible:ring-offset-ticket sm:w-auto"
                            disabled={!filteredSubscriptionURL}
                            onClick={() => void copySubscription()}
                            title={m.subscription.copyHint}
                        >
                            {copied ? <Check /> : <Copy />}
                            {copied
                                ? m.subscription.copiedShort
                                : m.subscription.copyShort}
                        </Button>
                    </div>
                </div>
                <p
                    aria-live="polite"
                    className={
                        copyMessage && !copied
                            ? "-mt-1 text-[0.8125rem] text-destructive"
                            : "sr-only"
                    }
                    role="status"
                >
                    {copyMessage}
                </p>
            </CardContent>
        </Card>
    );
}

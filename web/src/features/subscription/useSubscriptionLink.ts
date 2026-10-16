import { useMemo, useState } from "react";
import type { NodeRecord } from "@/types";
import { useCopyFeedback } from "@/hooks/useCopyFeedback";
import { useI18n } from "@/i18n";
import { compareGroupNames, groupNameCollator } from "@/lib/groups";

/** Protocols with a standard URI, the only ones the Base64 format can list. */
const uriProtocols = new Set(["socks5", "shadowsocks"]);

export function useSubscriptionLink(
    eligibleNodes: NodeRecord[],
    subscriptionURL: string,
    enabledProtocols: readonly string[] = ["socks5"],
) {
    // The selections are preferences. They apply only while the current nodes
    // and enabled protocols offer them, and come back if a reload makes them
    // available again.
    const { m } = useI18n();
    const [selectedProvider, setFilterProvider] = useState("");
    const [selectedGroup, setFilterGroup] = useState("");
    const [filterName, setFilterName] = useState("");
    const [format, setFormat] = useState<"" | "surge">("");
    const [selectedProtocol, setProtocol] = useState("socks5");
    const protocols = useMemo(
        () =>
            enabledProtocols.filter(
                (protocol) => format === "surge" || uriProtocols.has(protocol),
            ),
        [enabledProtocols, format],
    );
    const protocol = protocols.includes(selectedProtocol) ? selectedProtocol : "socks5";

    const nodeProviders = useMemo(
        () =>
            [...new Set(eligibleNodes.map((node) => node.provider))].sort(
                groupNameCollator.compare,
            ),
        [eligibleNodes],
    );
    // The select, the group options, and the URL all use this effective value.
    const filterProvider = nodeProviders.includes(selectedProvider)
        ? selectedProvider
        : "";
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
            if (format) next.searchParams.set("format", format);
            if (protocol !== "socks5") next.searchParams.set("protocol", protocol);
            return next.toString();
        } catch {
            return subscriptionURL;
        }
    }, [filterGroup, filterName, filterProvider, format, protocol, subscriptionURL]);

    // The copy message belongs to the copied URL and hides once it changes.
    const {
        message: copyMessage,
        copy: copySubscription,
        clear: clearCopyMessage,
    } = useCopyFeedback(filteredSubscriptionURL, m.subscription.copied);

    const resetFilters = () => {
        clearCopyMessage();
        setFilterProvider("");
        setFilterGroup("");
        setFilterName("");
    };

    return {
        copyMessage,
        copySubscription,
        filterGroup,
        filterGroups,
        filterName,
        filterProvider,
        filteredSubscriptionURL,
        format,
        nodeProviders,
        protocol,
        protocols,
        resetFilters,
        setFilterGroup,
        setFilterName,
        setFilterProvider,
        setFormat,
        setProtocol,
    };
}

import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { App } from "./App";
import { currentConsumerURL, makeApi, nodes, readyStatus } from "./test/fixtures";
import type { NodeRecord } from "./types";

const quickfoxNode: NodeRecord = {
  ...nodes[0], id: "quickfox_node", provider: "quickfox", name: "Seattle Edge", group: "Domestic / US",
};
const mixedNodes = [...nodes, quickfoxNode];

async function renderMixedProviders() {
  const status = readyStatus();
  status.accounts = {
    ...status.accounts,
    quickfox: { provider: "quickfox", display: "q-account", tier: "VIP", subscriptionActive: true },
  };
  const api = makeApi(status, mixedNodes);
  render(<App api={api} />);
  await screen.findByText(currentConsumerURL);
  return {
    api,
    updateNodes: async (nextNodes: NodeRecord[]) => {
      const before = vi.mocked(api.nodes).mock.calls.length;
      vi.mocked(api.nodes).mockResolvedValue({ nodes: nextNodes });
      const onEvent = vi.mocked(api.events).mock.calls[0][0];
      await act(async () => onEvent({ type: "state", state: "ready" }));
      await waitFor(() => expect(api.nodes).toHaveBeenCalledTimes(before + 1));
    },
  };
}

const providerSelect = () => screen.getByLabelText("Provider") as HTMLSelectElement;
const groupSelect = () => screen.getByLabelText("Group") as HTMLSelectElement;

function subscriptionURL(filters: Record<string, string> = {}) {
  const result = new URL(currentConsumerURL);
  for (const [key, value] of Object.entries(filters)) result.searchParams.set(key, value);
  return result.toString();
}

async function selectQuickfoxGroup() {
  await userEvent.selectOptions(providerSelect(), "quickfox");
  await userEvent.selectOptions(groupSelect(), "Domestic / US");
}

describe("subscription provider filter reconciliation", () => {
  it("drops unavailable provider and group filters when an SSE reload removes the account", async () => {
    const { api, updateNodes } = await renderMixedProviders();
    await selectQuickfoxGroup();
    await userEvent.type(screen.getByLabelText("Node name"), "Edge");
    expect(screen.getByText(subscriptionURL({ provider: "quickfox", group: "Domestic / US", name: "Edge" }))).toBeTruthy();

    api.statusValue = readyStatus();
    await updateNodes(nodes);

    const expected = subscriptionURL({ name: "Edge" });
    expect(await screen.findByText(expected)).toBeTruthy();
    expect(providerSelect().value).toBe("");
    expect(groupSelect().value).toBe("");
    expect(within(providerSelect()).queryByRole("option", { name: "QuickFox" })).toBeNull();
    expect(within(groupSelect()).queryByRole("option", { name: "Domestic / US" })).toBeNull();
    expect(within(groupSelect()).getByRole("option", { name: "East China" })).toBeTruthy();
    expect(within(groupSelect()).getByRole("option", { name: "West China" })).toBeTruthy();
    await userEvent.click(screen.getByRole("button", { name: "Copy subscription link" }));
    expect(navigator.clipboard.writeText).toHaveBeenLastCalledWith(expected);
  });

  it("uses All providers and All groups when the inventory has no eligible nodes", async () => {
    const { updateNodes } = await renderMixedProviders();
    await selectQuickfoxGroup();
    await updateNodes(mixedNodes.map((node) => ({ ...node, eligible: false })));

    expect(await screen.findByText(currentConsumerURL)).toBeTruthy();
    expect(providerSelect().value).toBe("");
    expect(groupSelect().value).toBe("");
    expect(within(providerSelect()).getAllByRole("option")).toHaveLength(1);
    expect(within(groupSelect()).getAllByRole("option")).toHaveLength(1);
    await userEvent.click(screen.getByRole("button", { name: "Copy subscription link" }));
    expect(navigator.clipboard.writeText).toHaveBeenLastCalledWith(currentConsumerURL);
  });

  it("retains still-valid provider and group choices when other nodes change", async () => {
    const { updateNodes } = await renderMixedProviders();
    await selectQuickfoxGroup();
    await userEvent.type(screen.getByLabelText("Node name"), " Edge ");
    await updateNodes([quickfoxNode, { ...nodes[0], group: "Changed group" }]);

    const expected = subscriptionURL({ provider: "quickfox", group: "Domestic / US", name: "Edge" });
    expect(await screen.findByText(expected)).toBeTruthy();
    expect(providerSelect().value).toBe("quickfox");
    expect(groupSelect().value).toBe("Domestic / US");
    expect(within(groupSelect()).queryByRole("option", { name: "Changed group" })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Copy subscription link" }));
    expect(navigator.clipboard.writeText).toHaveBeenLastCalledWith(expected);
  });

  it("restores the retained provider and group preferences together when they return", async () => {
    const { updateNodes } = await renderMixedProviders();
    await selectQuickfoxGroup();
    await updateNodes(nodes);
    expect(await screen.findByText(currentConsumerURL)).toBeTruthy();
    expect(providerSelect().value).toBe("");

    await updateNodes(mixedNodes);

    const expected = subscriptionURL({ provider: "quickfox", group: "Domestic / US" });
    expect(await screen.findByText(expected)).toBeTruthy();
    expect(providerSelect().value).toBe("quickfox");
    expect(groupSelect().value).toBe("Domestic / US");
    await userEvent.click(screen.getByRole("button", { name: "Copy subscription link" }));
    expect(navigator.clipboard.writeText).toHaveBeenLastCalledWith(expected);
  });

  it("does not restore an unavailable provider after the user chooses another provider", async () => {
    const { updateNodes } = await renderMixedProviders();
    await selectQuickfoxGroup();
    await updateNodes(nodes);
    expect(await screen.findByText(currentConsumerURL)).toBeTruthy();
    await userEvent.selectOptions(providerSelect(), "kuaifan");
    await userEvent.selectOptions(groupSelect(), "East China");
    await updateNodes(mixedNodes);

    const expected = subscriptionURL({ provider: "kuaifan", group: "East China" });
    expect(await screen.findByText(expected)).toBeTruthy();
    expect(providerSelect().value).toBe("kuaifan");
    expect(groupSelect().value).toBe("East China");
    expect(within(groupSelect()).queryByRole("option", { name: "Domestic / US" })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Copy subscription link" }));
    expect(navigator.clipboard.writeText).toHaveBeenLastCalledWith(expected);
  });
});

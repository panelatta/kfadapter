import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { App } from "./App";
import { ApiError } from "./api";
import { currentConsumerURL, makeApi, nodes, readyStatus, signedOutStatus } from "./test/fixtures";
import type { NodeRecord, StatusResponse } from "./types";

function accountStatus(display: string): StatusResponse {
  const status = readyStatus();
  status.accounts!.kuaifan.display = display;
  return status;
}

function emitState(api: ReturnType<typeof makeApi>) {
  const onEvent = vi.mocked(api.events).mock.calls[0][0];
  act(() => onEvent({ type: "state", state: "ready" }));
}

async function startSlowReload(api: ReturnType<typeof makeApi>) {
  const pending = Promise.withResolvers<StatusResponse>();
  const before = vi.mocked(api.status).mock.calls.length;
  vi.mocked(api.status).mockReturnValueOnce(pending.promise);
  emitState(api);
  await waitFor(() => expect(api.status).toHaveBeenCalledTimes(before + 1));
  return pending;
}

describe("authenticated reload ordering", () => {
  it("retains a completed explicit refresh when an older SSE status arrives", async () => {
    const api = makeApi();
    render(<App api={api} />);
    await screen.findByText(currentConsumerURL);
    const pending = await startSlowReload(api);

    api.statusValue = accountStatus("new-account");
    await userEvent.click(screen.getByRole("button", { name: "Refresh all" }));
    await screen.findByText("new-account");
    await act(async () => pending.resolve(readyStatus()));

    expect(screen.getByText("new-account")).toBeTruthy();
    expect(screen.queryByText("u•••@example.com")).toBeNull();
    // The obsolete status must not start a second wave of stale detail loads.
    expect(api.nodes).toHaveBeenCalledTimes(2);
    expect(api.subscriptionURL).toHaveBeenCalledTimes(2);
  });

  it("ignores older nodes and subscription results after a newer load completes", async () => {
    const api = makeApi();
    render(<App api={api} />);
    await screen.findByText(currentConsumerURL);
    const oldNodes = Promise.withResolvers<{ nodes: NodeRecord[] }>();
    const oldSubscription = Promise.withResolvers<{ url: string }>();
    vi.mocked(api.nodes).mockReturnValueOnce(oldNodes.promise);
    vi.mocked(api.subscriptionURL).mockReturnValueOnce(oldSubscription.promise);
    emitState(api);
    await waitFor(() => expect(api.nodes).toHaveBeenCalledTimes(2));

    const newURL = `${currentConsumerURL}-new`;
    api.statusValue = accountStatus("new-account");
    vi.mocked(api.nodes).mockResolvedValue({ nodes: [{ ...nodes[0], group: "Current group" }] });
    vi.mocked(api.subscriptionURL).mockResolvedValue({ url: newURL });
    await userEvent.click(screen.getByRole("button", { name: "Refresh all" }));
    await screen.findByText(newURL);
    await act(async () => {
      oldNodes.resolve({ nodes });
      oldSubscription.resolve({ url: currentConsumerURL });
    });

    expect(screen.getByText(newURL)).toBeTruthy();
    expect(screen.queryByText(currentConsumerURL)).toBeNull();
    expect(screen.getByRole("option", { name: "Current group" })).toBeTruthy();
    expect(screen.queryByRole("option", { name: "East China" })).toBeNull();
  });

  it.each(["status", "details"] as const)("ignores an obsolete %s authentication failure", async (stage) => {
    const api = makeApi();
    render(<App api={api} />);
    await screen.findByText(currentConsumerURL);
    const staleFailure = Promise.withResolvers<never>();
    if (stage === "status") vi.mocked(api.status).mockReturnValueOnce(staleFailure.promise);
    else vi.mocked(api.nodes).mockReturnValueOnce(staleFailure.promise);
    emitState(api);
    await waitFor(() => expect(stage === "status" ? api.status : api.nodes).toHaveBeenCalledTimes(2));

    api.statusValue = accountStatus("new-account");
    await userEvent.click(screen.getByRole("button", { name: "Refresh all" }));
    await screen.findByText("new-account");
    await act(async () => staleFailure.reject(new ApiError({ status: 401, code: "access_required", title: "Old session failure" })));

    expect(screen.getByText("new-account")).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Unlock console" })).toBeNull();
    expect(api.clearSession).not.toHaveBeenCalled();
  });

  it("keeps the newest load failure visible and permits a subsequent retry", async () => {
    const api = makeApi();
    render(<App api={api} />);
    await screen.findByText(currentConsumerURL);
    const pending = await startSlowReload(api);
    vi.mocked(api.status).mockRejectedValueOnce(new ApiError({ status: 503, code: "status_unavailable", title: "Latest load failed" }));
    await userEvent.click(screen.getByRole("button", { name: "Refresh all" }));
    await screen.findByText("Latest load failed");
    await act(async () => pending.resolve(accountStatus("obsolete-account")));

    expect(screen.getByText("Latest load failed")).toBeTruthy();
    expect(screen.queryByText("obsolete-account")).toBeNull();
    api.statusValue = accountStatus("recovered-account");
    await userEvent.click(screen.getByRole("button", { name: "Refresh all" }));
    await screen.findByText("recovered-account");
    expect(screen.queryByText("Latest load failed")).toBeNull();
  });

  it("does not restore a removed account when an older SSE load finishes", async () => {
    const api = makeApi();
    render(<App api={api} />);
    await screen.findByText(currentConsumerURL);
    const pending = await startSlowReload(api);
    api.statusValue = signedOutStatus();
    await userEvent.click(screen.getByRole("button", { name: "Sign out all" }));
    await userEvent.click(screen.getByRole("button", { name: "Confirm sign out" }));
    await screen.findByRole("heading", { name: "Connect account" });
    await act(async () => pending.resolve(readyStatus()));

    expect(screen.getByRole("heading", { name: "Connect account" })).toBeTruthy();
    expect(screen.queryByText(currentConsumerURL)).toBeNull();
    expect(api.nodes).toHaveBeenCalledTimes(1);
  });

  it("keeps a completed provider login when an older signed-out load arrives", async () => {
    const api = makeApi(signedOutStatus());
    render(<App api={api} />);
    await screen.findByRole("heading", { name: "Connect account" });
    const pending = await startSlowReload(api);
    await userEvent.selectOptions(screen.getByLabelText("Provider"), "kuaifan");
    await userEvent.type(screen.getByLabelText("Email"), "operator@example.com");
    await userEvent.type(screen.getByLabelText("Password"), "secret");
    await userEvent.click(screen.getByRole("button", { name: "Sign in" }));
    await screen.findByText(currentConsumerURL);
    await act(async () => pending.resolve(signedOutStatus()));

    expect(screen.getByRole("heading", { name: "Service status" })).toBeTruthy();
    expect(screen.getByText(currentConsumerURL)).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Connect account" })).toBeNull();
  });

  it("does not reload a replaced session after an older provider login finishes", async () => {
    const api = makeApi();
    render(<App api={api} />);
    await screen.findByText(currentConsumerURL);
    const pendingLogin = Promise.withResolvers<Awaited<ReturnType<typeof api.login>>>();
    vi.mocked(api.login).mockReturnValueOnce(pendingLogin.promise);
    await userEvent.click(screen.getByRole("button", { name: "QuickFox" }));
    await userEvent.type(screen.getByLabelText("Email"), "operator@example.com");
    await userEvent.type(screen.getByLabelText("Password"), "secret");
    await userEvent.click(screen.getByRole("button", { name: "Connect QuickFox" }));
    await waitFor(() => expect(api.login).toHaveBeenCalledOnce());
    await userEvent.click(screen.getByRole("button", { name: "Lock console" }));
    await screen.findByRole("heading", { name: "Unlock console" });
    await act(async () => pendingLogin.resolve(readyStatus().accounts!.kuaifan));

    expect(api.status).toHaveBeenCalledTimes(1);
    expect(screen.getByRole("heading", { name: "Unlock console" })).toBeTruthy();
  });
});

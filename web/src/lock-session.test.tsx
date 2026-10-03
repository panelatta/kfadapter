import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import { ApiClient } from "./api";
import { currentConsumerURL, disabledSmartProxy, nodes, readyStatus } from "./test/fixtures";
import { TestEventSource } from "./test/setup";

const accessToken = "valid-access-token-1234";

function response(payload: unknown, status = 200) {
  return new Response(status === 204 ? null : JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function sessionRequests() {
  const pendingLock = Promise.withResolvers<Response>();
  let lockRequests = 0;
  const fetchMock = vi.fn<(resource: RequestInfo | URL, init?: RequestInit) => Promise<Response>>((resource) => {
    const path = String(resource);
    if (path === "/api/v1/access/status")
      return Promise.resolve(response({ initialized: true, authenticated: true, csrfToken: "old-csrf" }));
    if (path === "/api/v1/access/logout") {
      lockRequests += 1;
      return lockRequests === 1 ? pendingLock.promise : Promise.resolve(response(null, 204));
    }
    if (path === "/api/v1/access/login")
      return Promise.resolve(response({ csrfToken: "new-csrf" }));
    if (path === "/api/v1/status") return Promise.resolve(response(readyStatus()));
    if (path === "/api/v1/nodes") return Promise.resolve(response({ nodes }));
    if (path === "/api/v1/smart-proxy") return Promise.resolve(response(disabledSmartProxy()));
    if (path === "/api/v1/subscription/url") return Promise.resolve(response({ url: currentConsumerURL }));
    if (path === "/api/v1/control/refresh") return Promise.resolve(response(null, 204));
    throw new Error(`Unexpected request: ${path}`);
  });
  vi.stubGlobal("fetch", fetchMock);
  const calls = (path: string) => fetchMock.mock.calls.filter(([resource]) => resource === `/api/v1/${path}`);
  return { pendingLock, calls };
}

async function unlock() {
  await userEvent.type(await screen.findByLabelText("Access token"), accessToken);
  await userEvent.click(screen.getByRole("button", { name: "Continue" }));
  await screen.findByText(currentConsumerURL);
}

afterEach(() => vi.unstubAllGlobals());

describe("console lock and unlock ordering", () => {
  it("keeps login unavailable until a delayed logout response has finished", async () => {
    const { pendingLock, calls } = sessionRequests();
    render(<App api={new ApiClient()} />);
    await screen.findByText(currentConsumerURL);
    const source = TestEventSource.instances[0];

    await userEvent.click(screen.getByRole("button", { name: "Lock console" }));
    expect(screen.getByRole("heading", { name: "Locking console" })).toBeTruthy();
    expect(screen.queryByLabelText("Access token")).toBeNull();
    expect(screen.queryByRole("button", { name: "Continue" })).toBeNull();
    expect(screen.queryByText(currentConsumerURL)).toBeNull();
    expect(calls("access/login")).toHaveLength(0);
    expect(calls("access/logout")).toHaveLength(1);
    expect(source.closed).toBe(true);

    await act(async () => pendingLock.resolve(new Response(null, {
      status: 204,
      headers: { "Set-Cookie": "kfadapter_session=; Max-Age=0; Path=/; HttpOnly; SameSite=Strict" },
    })));
    await unlock();
    await userEvent.click(screen.getByRole("button", { name: "Refresh all" }));
    await waitFor(() => expect(calls("control/refresh")).toHaveLength(1));
    expect(calls("control/refresh")[0][1]?.headers).toMatchObject({ "X-CSRF-Token": "new-csrf" });
    expect(calls("access/login")).toHaveLength(1);
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it.each(["network", "http"] as const)("shows a delayed %s lock failure and allows unlocking and locking again", async (failure) => {
    const { pendingLock, calls } = sessionRequests();
    render(<App api={new ApiClient()} />);
    await screen.findByText(currentConsumerURL);
    await userEvent.click(screen.getByRole("button", { name: "Lock console" }));
    expect(screen.queryByLabelText("Access token")).toBeNull();
    expect(calls("access/login")).toHaveLength(0);

    await act(async () => {
      if (failure === "network") pendingLock.reject(new TypeError("Network unavailable"));
      else pendingLock.resolve(response({ title: "Lock unavailable", code: "access_unavailable" }, 503));
    });
    expect(await screen.findByRole("heading", { name: "Unlock console" })).toBeTruthy();
    expect(screen.getByRole("alert").textContent).toContain("could not confirm the lock");

    await unlock();
    expect(screen.queryByRole("alert")).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Refresh all" }));
    await waitFor(() => expect(calls("control/refresh")).toHaveLength(1));
    expect(calls("control/refresh")[0][1]?.headers).toMatchObject({ "X-CSRF-Token": "new-csrf" });
    await userEvent.click(screen.getByRole("button", { name: "Lock console" }));
    await screen.findByRole("heading", { name: "Unlock console" });
    expect(calls("access/logout")).toHaveLength(2);
    expect(calls("access/logout")[1][1]?.headers).toMatchObject({ "X-CSRF-Token": "new-csrf" });
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("explains that a failed durable lock can return after restart and permits a recovery lock", async () => {
    const { pendingLock, calls } = sessionRequests();
    render(<App api={new ApiClient()} />);
    await screen.findByText(currentConsumerURL);
    await userEvent.click(screen.getByRole("button", { name: "Lock console" }));
    await act(async () => pendingLock.resolve(response({
      title: "Session lock could not be saved",
      code: "session_revocation_pending",
    }, 503)));

    await screen.findByRole("heading", { name: "Unlock console" });
    const warning = screen.getByRole("alert").textContent;
    expect(warning).toContain("The console is locked now");
    expect(warning).toContain("could become valid again after the service restarts");
    expect(warning).toContain("Repair persistent storage, then unlock and lock the console again to retry");
    expect(screen.queryByText(currentConsumerURL)).toBeNull();
    expect(calls("access/logout")).toHaveLength(1);

    await unlock();
    await userEvent.click(screen.getByRole("button", { name: "Lock console" }));
    await screen.findByRole("heading", { name: "Unlock console" });
    expect(calls("access/logout")).toHaveLength(2);
    expect(calls("access/logout")[1][1]?.headers).toMatchObject({ "X-CSRF-Token": "new-csrf" });
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("finishes an unauthorized lock before allowing a fresh session", async () => {
    const { pendingLock, calls } = sessionRequests();
    render(<App api={new ApiClient()} />);
    await screen.findByText(currentConsumerURL);
    await userEvent.click(screen.getByRole("button", { name: "Lock console" }));
    await act(async () => pendingLock.resolve(response({ title: "Already locked", code: "access_required" }, 401)));
    await unlock();
    await userEvent.click(screen.getByRole("button", { name: "Refresh all" }));
    await waitFor(() => expect(calls("control/refresh")).toHaveLength(1));
    expect(calls("control/refresh")[0][1]?.headers).toMatchObject({ "X-CSRF-Token": "new-csrf" });
    expect(screen.queryByRole("alert")).toBeNull();
  });
});

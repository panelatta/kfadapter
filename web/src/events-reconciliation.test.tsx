import { act, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import { ApiClient } from "./api";
import { currentConsumerURL, disabledSmartProxy, nodes, readyStatus } from "./test/fixtures";
import { TestEventSource } from "./test/setup";

function response(payload: unknown, status = 200) {
  return new Response(JSON.stringify(payload), { status, headers: { "Content-Type": "application/json" } });
}

function mountConsole(firstStatus?: Promise<Response>) {
  let status = readyStatus();
  let inventory = nodes;
  let subscriptionURL = currentConsumerURL;
  let statusFailure = 0;
  let statusRequests = 0;
  const fetchMock = vi.fn((resource: RequestInfo | URL): Promise<Response> => {
    const path = String(resource);
    if (path === "/api/v1/access/status")
      return Promise.resolve(response({ initialized: true, authenticated: true, csrfToken: "csrf-test" }));
    if (path === "/api/v1/status") {
      statusRequests += 1;
      if (statusRequests === 1 && firstStatus) return firstStatus;
      if (statusFailure) return Promise.resolve(response({ title: "Reconciliation unavailable", code: "status_unavailable" }, statusFailure));
      return Promise.resolve(response(status));
    }
    if (path === "/api/v1/nodes") return Promise.resolve(response({ nodes: inventory }));
    if (path === "/api/v1/smart-proxy") return Promise.resolve(response(disabledSmartProxy()));
    if (path === "/api/v1/subscription/url") return Promise.resolve(response({ url: subscriptionURL }));
    throw new Error(`Unexpected request: ${path}`);
  });
  vi.stubGlobal("fetch", fetchMock);
  const rendered = render(<App />);
  return {
    ...rendered,
    fetchMock,
    statusRequests: () => statusRequests,
    setFailure: (code: number) => { statusFailure = code; },
    update: () => {
      status = { ...readyStatus(), state: "degraded" };
      inventory = [{ ...nodes[0], group: "Updated group" }];
      subscriptionURL = `${currentConsumerURL}-updated`;
    },
  };
}

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("event stream authoritative reconciliation", () => {
  it("queues a fresh load when the first stream opens while the initial load is pending", async () => {
    const pending = Promise.withResolvers<Response>();
    const console = mountConsole(pending.promise);
    await waitFor(() => expect(TestEventSource.instances).toHaveLength(1));
    expect(console.statusRequests()).toBe(1);
    const source = TestEventSource.instances[0];
    console.update();

    act(() => {
      source.open();
      source.open();
      source.emit("state", JSON.stringify({ state: "degraded" }));
      source.emit("refresh", JSON.stringify({ state: "degraded", complete: true }));
    });
    expect(console.statusRequests()).toBe(1);
    await act(async () => pending.resolve(response(readyStatus())));

    await screen.findByText("Needs attention", { exact: true });
    expect(console.statusRequests()).toBe(2);
    expect(screen.queryByText("Ready", { exact: true })).toBeNull();
  });

  it("recovers changes between the initial load and the first open without duplicate open reloads", async () => {
    const console = mountConsole();
    await screen.findByText(currentConsumerURL);
    const source = TestEventSource.instances[0];
    console.update();
    await act(async () => source.open());
    await screen.findByText(`${currentConsumerURL}-updated`);
    expect(screen.getByText("Needs attention", { exact: true })).toBeTruthy();
    expect(screen.getByRole("option", { name: "Updated group" })).toBeTruthy();
    expect(console.statusRequests()).toBe(2);

    await act(async () => {
      source.open();
      source.open();
    });
    expect(console.statusRequests()).toBe(2);
  });

  it.each([false, true])("reloads a browser-managed reconnection after a failed outage reload: %s", async (failed) => {
    const console = mountConsole();
    await screen.findByText(currentConsumerURL);
    const source = TestEventSource.instances[0];
    await act(async () => source.open());
    expect(console.statusRequests()).toBe(2);
    if (failed) console.setFailure(503);
    await act(async () => source.fail());
    expect(console.statusRequests()).toBe(3);
    if (failed) expect(screen.getByText("Reconciliation unavailable")).toBeTruthy();

    // These changes occur after the best-effort reload during the outage. No
    // event will be replayed when this same EventSource instance reconnects.
    console.setFailure(0);
    console.update();
    await act(async () => source.open());
    await screen.findByText(`${currentConsumerURL}-updated`);

    expect(console.statusRequests()).toBe(4);
    expect(screen.getByText("Needs attention", { exact: true })).toBeTruthy();
    expect(screen.getByRole("option", { name: "Updated group" })).toBeTruthy();
    expect(screen.queryByText("Reconciliation unavailable")).toBeNull();
    expect(TestEventSource.instances).toHaveLength(1);
    expect(source.close).not.toHaveBeenCalled();
  });

  it("closes the stream after an open reconciles to an expired session and ignores late callbacks", async () => {
    const console = mountConsole();
    await screen.findByText(currentConsumerURL);
    const source = TestEventSource.instances[0];
    const lateOpen = source.onopen!;
    const lateError = source.onerror!;
    const lateState = [...source.listeners.get("state")!][0];
    console.setFailure(401);
    await act(async () => source.open());
    await screen.findByRole("heading", { name: "Unlock console" });
    expect(source.closed).toBe(true);
    expect(screen.queryByText(currentConsumerURL)).toBeNull();
    const requests = console.fetchMock.mock.calls.length;

    await act(async () => {
      lateOpen(new Event("open"));
      lateError(new Event("error"));
      lateState(new MessageEvent("state", { data: JSON.stringify({ state: "ready" }) }));
      source.open();
      source.fail();
    });
    expect(console.fetchMock.mock.calls).toHaveLength(requests);
    expect(TestEventSource.instances).toHaveLength(1);
  });

  it("preserves CLOSED backoff and reconciles newly created streams, then stops on close", () => {
    vi.useFakeTimers();
    const onEvent = vi.fn();
    const onFailure = vi.fn();
    const onOpen = vi.fn();
    const close = new ApiClient().events(onEvent, onFailure, onOpen);
    const first = TestEventSource.instances[0];
    first.reject();
    vi.advanceTimersByTime(999);
    expect(TestEventSource.instances).toHaveLength(1);
    vi.advanceTimersByTime(1);
    const second = TestEventSource.instances[1];
    second.reject();
    vi.advanceTimersByTime(1999);
    expect(TestEventSource.instances).toHaveLength(2);
    vi.advanceTimersByTime(1);
    const third = TestEventSource.instances[2];
    third.open();
    third.open();
    expect(onOpen).toHaveBeenCalledTimes(1);
    third.fail();
    third.open();
    expect(onOpen).toHaveBeenCalledTimes(2);
    third.reject();
    vi.advanceTimersByTime(1000);
    const fourth = TestEventSource.instances[3];
    fourth.open();
    expect(onOpen).toHaveBeenCalledTimes(3);
    fourth.reject();
    close();
    vi.runAllTimers();
    expect(TestEventSource.instances).toHaveLength(4);
    expect(onFailure).toHaveBeenCalledTimes(5);
    expect(onEvent).not.toHaveBeenCalled();
  });
});

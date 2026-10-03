import { act, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "./api";
import { SmartProxyCard } from "./components/SmartProxyCard";
import { makeApi } from "./test/fixtures";
import type { SmartProxyStatus } from "./types";

function status(enabled = true): SmartProxyStatus {
  return { enabled, intervalMinutes: 30, running: false, targets: [], results: [] };
}

const originalExecCommand = Object.getOwnPropertyDescriptor(document, "execCommand");

async function mount(initialFailure?: Error) {
  vi.useFakeTimers();
  const api = makeApi();
  vi.mocked(api.smartProxy).mockResolvedValue(status());
  if (initialFailure) vi.mocked(api.smartProxy).mockRejectedValueOnce(initialFailure);
  let revision = 0;
  const lost = vi.fn();
  const rendered = render(<SmartProxyCard api={api} getSessionRevision={() => revision} onAccessLost={lost} />);
  await act(async () => {});
  return { api, lost, ...rendered, expire: () => { revision += 1; } };
}

afterEach(() => {
  vi.useRealTimers();
  if (originalExecCommand) Object.defineProperty(document, "execCommand", originalExecCommand);
  else Reflect.deleteProperty(document, "execCommand");
});

describe("smart proxy asynchronous state", () => {
  it.each([401, 503])("ignores an obsolete poll %s after a successful configuration change", async (code) => {
    const { api, lost } = await mount();
    const pending = Promise.withResolvers<SmartProxyStatus>();
    vi.mocked(api.smartProxy).mockReturnValueOnce(pending.promise);
    await act(async () => vi.advanceTimersByTimeAsync(5000));
    await act(async () => fireEvent.click(screen.getByRole("switch")));
    expect((screen.getByRole("switch") as HTMLInputElement).checked).toBe(false);
    await act(async () => pending.reject(new ApiError({ status: code, code: "old_poll_failure", title: "Obsolete poll failure" })));

    expect(lost).not.toHaveBeenCalled();
    expect(screen.queryByRole("alert")).toBeNull();
    expect((screen.getByRole("switch") as HTMLInputElement).checked).toBe(false);
  });

  it("clears a transient polling error after the next successful status response", async () => {
    const { api } = await mount();
    vi.mocked(api.smartProxy).mockRejectedValueOnce(new ApiError({ status: 503, code: "poll_unavailable", title: "Polling failed" }));
    await act(async () => vi.advanceTimersByTimeAsync(5000));
    expect(screen.getByRole("alert").textContent).toBe("Polling failed");
    await act(async () => vi.advanceTimersByTimeAsync(5000));
    expect(screen.queryByRole("alert")).toBeNull();
    expect((screen.getByRole("switch") as HTMLInputElement).disabled).toBe(false);
  });

  it("recovers from a failed initial load without leaving the form disabled or showing a stale error", async () => {
    const { api } = await mount(new Error("Offline"));
    expect(screen.getByText("Smart proxy status is unavailable. Retrying…")).toBeTruthy();
    expect((screen.getByRole("switch") as HTMLInputElement).disabled).toBe(true);
    await act(async () => vi.advanceTimersByTimeAsync(5000));
    expect(api.smartProxy).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole("alert")).toBeNull();
    expect((screen.getByRole("switch") as HTMLInputElement).disabled).toBe(false);
  });

  it("preserves an action failure through successful polling, then allows retry", async () => {
    const { api } = await mount();
    vi.mocked(api.configureSmartProxy).mockRejectedValueOnce(new ApiError({ status: 503, code: "configure_failed", title: "Settings were not saved" }));
    await act(async () => fireEvent.click(screen.getByRole("switch")));
    await act(async () => vi.advanceTimersByTimeAsync(5000));
    expect(screen.getByRole("alert").textContent).toBe("Settings were not saved");
    expect((screen.getByRole("switch") as HTMLInputElement).checked).toBe(true);
    await act(async () => fireEvent.click(screen.getByRole("switch")));
    expect(screen.queryByRole("alert")).toBeNull();
    expect((screen.getByRole("switch") as HTMLInputElement).checked).toBe(false);
  });

  it("stops polling and issuing actions after the browser session changes", async () => {
    const { api, expire } = await mount();
    expire();
    await act(async () => vi.advanceTimersByTimeAsync(15000));
    await act(async () => fireEvent.click(screen.getByRole("switch")));
    expect(api.smartProxy).toHaveBeenCalledOnce();
    expect(api.configureSmartProxy).not.toHaveBeenCalled();
  });

  it("does not let polling revoke access while a configuration request is pending", async () => {
    const { api, lost } = await mount();
    const pending = Promise.withResolvers<SmartProxyStatus>();
    vi.mocked(api.configureSmartProxy).mockReturnValueOnce(pending.promise);
    await act(async () => fireEvent.click(screen.getByRole("switch")));
    vi.mocked(api.smartProxy).mockRejectedValue(new ApiError({ status: 401, code: "access_required", title: "Expired" }));
    await act(async () => vi.advanceTimersByTimeAsync(10000));
    expect(api.smartProxy).toHaveBeenCalledOnce();
    expect(lost).not.toHaveBeenCalled();
    await act(async () => pending.resolve(status(false)));
    vi.mocked(api.smartProxy).mockResolvedValue(status(false));
    await act(async () => vi.advanceTimersByTimeAsync(5000));
    expect(api.smartProxy).toHaveBeenCalledTimes(2);
    expect((screen.getByRole("switch") as HTMLInputElement).checked).toBe(false);
  });

  it("stops on a current 401 instead of retrying expired access every five seconds", async () => {
    const { api, lost } = await mount();
    vi.mocked(api.smartProxy).mockRejectedValue(new ApiError({ status: 401, code: "access_required", title: "Expired" }));
    await act(async () => vi.advanceTimersByTimeAsync(15000));
    expect(lost).toHaveBeenCalledOnce();
    expect(api.smartProxy).toHaveBeenCalledTimes(2);
  });

  it("does not start a legacy clipboard fallback if the native copy rejects after locking", async () => {
    const { expire } = await mount();
    const pending = Promise.withResolvers<void>();
    vi.mocked(navigator.clipboard.writeText).mockReturnValueOnce(pending.promise);
    const legacyCopy = vi.fn().mockReturnValue(true);
    Object.defineProperty(document, "execCommand", { configurable: true, value: legacyCopy });
    await act(async () => fireEvent.click(screen.getByRole("button", { name: "Copy smart SOCKS5 address" })));
    expect(navigator.clipboard.writeText).toHaveBeenCalledOnce();
    expire();
    await act(async () => pending.reject(new Error("Permission denied")));
    expect(legacyCopy).not.toHaveBeenCalled();
    expect(screen.getByRole("status").textContent).toBe("");
  });

  it("still falls back to legacy copying when the current session's native copy is denied", async () => {
    await mount();
    vi.mocked(navigator.clipboard.writeText).mockRejectedValueOnce(new Error("Permission denied"));
    const legacyCopy = vi.fn().mockReturnValue(true);
    Object.defineProperty(document, "execCommand", { configurable: true, value: legacyCopy });
    await act(async () => fireEvent.click(screen.getByRole("button", { name: "Copy smart SOCKS5 address" })));
    expect(legacyCopy).toHaveBeenCalledWith("copy");
    expect(screen.getByRole("status").textContent).toBe("Smart SOCKS5 address copied.");
    expect(document.querySelector("textarea")).toBeNull();
  });

  it("does not start a fallback or another poll after unmount", async () => {
    const { api, unmount } = await mount();
    const pending = Promise.withResolvers<void>();
    vi.mocked(navigator.clipboard.writeText).mockReturnValueOnce(pending.promise);
    const legacyCopy = vi.fn().mockReturnValue(true);
    Object.defineProperty(document, "execCommand", { configurable: true, value: legacyCopy });
    await act(async () => fireEvent.click(screen.getByRole("button", { name: "Copy smart SOCKS5 address" })));
    unmount();
    await act(async () => pending.reject(new Error("Permission denied")));
    await act(async () => vi.advanceTimersByTimeAsync(15000));
    expect(legacyCopy).not.toHaveBeenCalled();
    expect(api.smartProxy).toHaveBeenCalledOnce();
  });
});

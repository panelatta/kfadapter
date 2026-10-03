import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { SmartProxyCard } from "./components/SmartProxyCard";
import { makeApi } from "./test/fixtures";
import { ApiError } from "./api";

function mount() {
    const api = makeApi();
    let revision = 0;
    const lost = vi.fn();
    const rendered = render(<SmartProxyCard api={api} getSessionRevision={() => revision} onAccessLost={lost} />);
    return { api, lost, ...rendered, expire: () => { revision += 1; } };
}

describe("smart SOCKS5 control", () => {
    it("enables, changes interval, probes and copies credentials only on demand", async () => {
        const { api } = mount();
        const toggle = await screen.findByRole("switch", { name: "Enable smart SOCKS5 proxy" });
        await waitFor(() => expect((toggle as HTMLInputElement).disabled).toBe(false));
        expect(api.smartProxyDetails).not.toHaveBeenCalled();
        await userEvent.click(toggle);
        expect(api.configureSmartProxy).toHaveBeenLastCalledWith(true, 30);
        await userEvent.selectOptions(screen.getByLabelText("Smart proxy probe interval"), "60");
        expect(api.configureSmartProxy).toHaveBeenLastCalledWith(true, 60);
        await userEvent.click(screen.getByRole("button", { name: "Copy smart SOCKS5 address" }));
        expect(api.smartProxyDetails).toHaveBeenCalledOnce();
        expect(navigator.clipboard.writeText).toHaveBeenCalledWith("socks5://automatic:secret@127.0.0.1:10808");
        await userEvent.click(screen.getByRole("button", { name: "Probe now" }));
        expect(api.probeSmartProxy).toHaveBeenCalledOnce();
        expect((screen.getByRole("button", { name: "Probing…" }) as HTMLButtonElement).disabled).toBe(true);
        await userEvent.click(toggle);
        expect(api.configureSmartProxy).toHaveBeenLastCalledWith(false, 60);
        expect((screen.getByRole("button", { name: "Copy smart SOCKS5 address" }) as HTMLButtonElement).disabled).toBe(true);
    });

    it("keeps saved settings on failure and reports lost access", async () => {
        const { api, lost } = mount();
        const toggle = await screen.findByRole("switch", { name: "Enable smart SOCKS5 proxy" });
        await waitFor(() => expect((toggle as HTMLInputElement).disabled).toBe(false));
        vi.mocked(api.configureSmartProxy).mockRejectedValueOnce(new Error("offline"));
        await userEvent.click(toggle);
        expect((toggle as HTMLInputElement).checked).toBe(false);
        expect(await screen.findByRole("alert")).toBeTruthy();
        vi.mocked(api.configureSmartProxy).mockRejectedValueOnce(new ApiError({ title: "Expired", status: 401, code: "not_authenticated" }));
        await userEvent.click(toggle);
        expect(lost).toHaveBeenCalledOnce();
    });

    it("does not copy a credential response from an expired browser session", async () => {
        const { api, expire } = mount();
        const toggle = await screen.findByRole("switch", { name: "Enable smart SOCKS5 proxy" });
        await waitFor(() => expect((toggle as HTMLInputElement).disabled).toBe(false));
        await userEvent.click(toggle);
        const pending = Promise.withResolvers<Awaited<ReturnType<typeof api.smartProxyDetails>>>();
        vi.mocked(api.smartProxyDetails).mockReturnValueOnce(pending.promise);
        await userEvent.click(screen.getByRole("button", { name: "Copy smart SOCKS5 address" }));
        expire();
        pending.resolve({ url: "socks5://secret", socksAddress: "host", socksUsername: "user", socksPassword: "secret" });
        await pending.promise;
        expect(navigator.clipboard.writeText).not.toHaveBeenCalled();
    });
});

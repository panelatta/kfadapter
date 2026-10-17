import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { App } from "./App";
import { ApiError } from "./api";
import { SmartProxyCard } from "./features/smart/SmartProxyCard";
import { chengduDetails, currentConsumerURL, inboundsStatus, makeApi } from "./test/fixtures";

async function renderConsole() {
    const api = makeApi();
    render(<App api={api} />);
    await screen.findByText(currentConsumerURL);
    const list = await screen.findByRole("list", { name: "Inbound protocols" });
    return { api, list };
}

const switchFor = (name: string) =>
    screen.getByRole("switch", { name: `Enable ${name}` }) as HTMLInputElement;

describe("inbound protocols", () => {
    it("lists every protocol with its port and enables one at a time", async () => {
        const { api, list } = await renderConsole();
        const rows = within(list).getAllByRole("listitem");
        expect(rows.map((row) => row.textContent)).toEqual([
            expect.stringContaining("SOCKS5TCP 10808"),
            expect.stringContaining("Snell v6TCP 10810"),
            expect.stringContaining("Shadowsocks 2022TCP 10811"),
            expect.stringContaining("SOCKS5-TLSTCP 10812"),
        ]);
        expect(within(rows[0]).getByText("Listening")).toBeTruthy();
        expect(switchFor("SOCKS5").checked).toBe(true);
        expect(screen.queryByText(/Certificate fingerprint/)).toBeNull();

        await userEvent.click(switchFor("Snell v6"));
        expect(api.configureInbounds).toHaveBeenLastCalledWith({ socks5: true, snell: true, shadowsocks: false, socksTLS: false });
        await waitFor(() => expect(switchFor("Snell v6").checked).toBe(true));
        expect(within(rows[1]).getByText("Listening")).toBeTruthy();

        await userEvent.click(switchFor("SOCKS5-TLS"));
        expect(api.configureInbounds).toHaveBeenLastCalledWith({ socks5: true, snell: true, shadowsocks: false, socksTLS: true });
        expect(await screen.findByText(/Certificate fingerprint \(SHA-256\): 8bd0624728/)).toBeTruthy();

        await userEvent.click(switchFor("SOCKS5"));
        expect(api.configureInbounds).toHaveBeenLastCalledWith({ socks5: false, snell: true, shadowsocks: false, socksTLS: true });
        await waitFor(() => expect(switchFor("SOCKS5").checked).toBe(false));
        expect(within(rows[0]).getByText("Off")).toBeTruthy();
    });

    it("keeps a protocol off when its port cannot be bound", async () => {
        const { api } = await renderConsole();
        vi.mocked(api.configureInbounds).mockRejectedValueOnce(
            new ApiError({ title: "Conflict", status: 409, code: "inbound_listen_failed" }),
        );
        await userEvent.click(switchFor("Shadowsocks 2022"));
        expect((await screen.findByRole("alert")).textContent).toContain("That port is in use");
        expect(switchFor("Shadowsocks 2022").checked).toBe(false);
    });

    it("reports a listener that failed to start", async () => {
        const api = makeApi();
        const failing = inboundsStatus({ socks5: true, snell: true, shadowsocks: false, socksTLS: false });
        failing.inbounds[1] = { ...failing.inbounds[1], listening: false, problem: "listen_failed" };
        vi.mocked(api.inbounds).mockResolvedValue(failing);
        render(<App api={api} />);
        expect(await screen.findByText("Port unavailable")).toBeTruthy();
    });

    it("builds Surge and per-protocol subscription links from enabled protocols", async () => {
        const { api } = await renderConsole();
        const protocol = () => screen.getByLabelText("Protocol") as HTMLSelectElement;
        const options = () => [...protocol().options].map((option) => option.value);
        expect(options()).toEqual(["socks5"]);

        vi.mocked(api.configureInbounds).mockResolvedValueOnce(inboundsStatus({ socks5: true, snell: true, shadowsocks: true, socksTLS: false }));
        await userEvent.click(switchFor("Snell v6"));
        // Base64 lists only protocols with a standard URI.
        await waitFor(() => expect(options()).toEqual(["socks5", "shadowsocks"]));
        await userEvent.selectOptions(protocol(), "shadowsocks");
        expect(screen.getByText(`${currentConsumerURL}?protocol=shadowsocks`)).toBeTruthy();

        await userEvent.selectOptions(screen.getByLabelText("Format"), "surge");
        expect(options()).toEqual(["socks5", "snell", "shadowsocks"]);
        await userEvent.selectOptions(protocol(), "snell");
        expect(screen.getByText(`${currentConsumerURL}?format=surge&protocol=snell`)).toBeTruthy();
        expect(screen.getByText(/policy-path/)).toBeTruthy();

        // Back to Base64, Snell falls back to SOCKS5.
        await userEvent.selectOptions(screen.getByLabelText("Format"), "");
        expect(protocol().value).toBe("socks5");
        expect(screen.getByText(currentConsumerURL)).toBeTruthy();
    });

    it("falls back from SOCKS5 in the subscription link once it is off", async () => {
        const { api } = await renderConsole();
        const protocol = () => screen.getByLabelText("Protocol") as HTMLSelectElement;
        vi.mocked(api.configureInbounds).mockResolvedValueOnce(inboundsStatus({ socks5: false, snell: true, shadowsocks: true, socksTLS: false }));
        await userEvent.click(switchFor("SOCKS5"));
        await waitFor(() => expect(protocol().value).toBe("shadowsocks"));
        expect(screen.getByText(`${currentConsumerURL}?protocol=shadowsocks`)).toBeTruthy();

        // Snell has no URI, so Base64 has nothing to list without SOCKS5 or
        // Shadowsocks 2022; Surge still does.
        vi.mocked(api.configureInbounds).mockResolvedValueOnce(inboundsStatus({ socks5: false, snell: true, shadowsocks: false, socksTLS: false }));
        await userEvent.click(switchFor("Shadowsocks 2022"));
        expect(await screen.findByText(/Base64 links list only SOCKS5 or Shadowsocks 2022/)).toBeTruthy();
        expect(protocol().disabled).toBe(true);
        expect((screen.getByRole("button", { name: "Copy subscription link" }) as HTMLButtonElement).disabled).toBe(true);
        await userEvent.selectOptions(screen.getByLabelText("Format"), "surge");
        expect(screen.getByText(`${currentConsumerURL}?format=surge&protocol=snell`)).toBeTruthy();
    });

    it("copies a node's Surge line and ss:// URL", async () => {
        const { api } = await renderConsole();
        vi.mocked(api.nodeDetails).mockResolvedValueOnce({
            ...chengduDetails,
            profiles: [
                { protocol: "socks5", port: 10808, surge: "Chengdu 02 = socks5, 127.0.0.1, 10808, u, p", uri: "socks5://u:p@127.0.0.1:10808" },
                { protocol: "snell", port: 10810, surge: "Chengdu 02 = snell, 127.0.0.1, 10810, psk=k, version=6" },
                { protocol: "shadowsocks", port: 10811, surge: "Chengdu 02 = ss, 127.0.0.1, 10811", uri: "ss://2022-blake3-aes-128-gcm:a%3Ab@127.0.0.1:10811" },
            ],
        });
        await userEvent.click(screen.getByRole("button", { name: "KuaiFan" }));
        await userEvent.click((await screen.findAllByRole("button", { name: "Details for Chengdu 02" }))[0]);
        await userEvent.click(await screen.findByRole("button", { name: "Copy Surge line for Snell v6" }));
        expect(navigator.clipboard.writeText).toHaveBeenCalledWith("Chengdu 02 = snell, 127.0.0.1, 10810, psk=k, version=6");
        expect(await screen.findByText("Surge line copied.")).toBeTruthy();
        await userEvent.click(screen.getByRole("button", { name: "Copy ss:// URL" }));
        expect(navigator.clipboard.writeText).toHaveBeenLastCalledWith("ss://2022-blake3-aes-128-gcm:a%3Ab@127.0.0.1:10811");
    });

    it("offers smart route copy buttons for each enabled protocol", async () => {
        const api = makeApi();
        vi.mocked(api.smartProxy).mockResolvedValue({ enabled: true, intervalMinutes: 30, running: false, targets: [], results: [] });
        vi.mocked(api.smartProxyDetails).mockResolvedValue({
            url: "socks5://a:b@127.0.0.1:10808", socksAddress: "127.0.0.1:10808", socksUsername: "a", socksPassword: "b",
            profiles: [
                { protocol: "socks5", port: 10808, surge: "智能线路 = socks5, 127.0.0.1, 10808, a, b", uri: "socks5://a:b@127.0.0.1:10808" },
                { protocol: "snell", port: 10810, surge: "智能线路 = snell, 127.0.0.1, 10810, psk=s" },
                { protocol: "shadowsocks", port: 10811, surge: "智能线路 = ss, 127.0.0.1, 10811", uri: "ss://2022-blake3-aes-128-gcm:a%3Ab@127.0.0.1:10811" },
            ],
        });
        const { rerender } = render(<SmartProxyCard api={api} getSessionRevision={() => 0} onAccessLost={() => {}} />);
        const socks = await screen.findByRole("button", { name: "Copy smart route SOCKS5 URL" });
        await waitFor(() => expect((socks as HTMLButtonElement).disabled).toBe(false));
        expect(screen.getByRole("button", { name: "Copy smart route Surge line for SOCKS5" })).toBeTruthy();
        expect(screen.queryByRole("button", { name: "Copy smart route Surge line for Snell v6" })).toBeNull();
        expect(screen.queryByRole("button", { name: "Copy smart route ss:// URL" })).toBeNull();

        rerender(<SmartProxyCard api={api} getSessionRevision={() => 0} onAccessLost={() => {}} protocols={["socks5", "snell", "shadowsocks"]} />);
        await userEvent.click(screen.getByRole("button", { name: "Copy smart route Surge line for Snell v6" }));
        expect(navigator.clipboard.writeText).toHaveBeenCalledWith("智能线路 = snell, 127.0.0.1, 10810, psk=s");
        expect(await screen.findByText("Smart route Surge line for Snell v6 copied.")).toBeTruthy();
        await userEvent.click(screen.getByRole("button", { name: "Copy smart route ss:// URL" }));
        expect(navigator.clipboard.writeText).toHaveBeenLastCalledWith("ss://2022-blake3-aes-128-gcm:a%3Ab@127.0.0.1:10811");
        expect(await screen.findByText("Smart route ss:// URL copied.")).toBeTruthy();
    });

    it("reports a smart route protocol that the server no longer offers", async () => {
        const api = makeApi();
        vi.mocked(api.smartProxy).mockResolvedValue({ enabled: true, intervalMinutes: 30, running: false, targets: [], results: [] });
        vi.mocked(api.smartProxyDetails).mockResolvedValue({
            url: "socks5://a:b@127.0.0.1:10808", socksAddress: "127.0.0.1:10808", socksUsername: "a", socksPassword: "b",
            profiles: [{ protocol: "socks5", port: 10808, surge: "智能线路 = socks5, 127.0.0.1, 10808, a, b" }],
        });
        render(<SmartProxyCard api={api} getSessionRevision={() => 0} onAccessLost={() => {}} protocols={["socks5", "socks5-tls"]} />);
        const tls = await screen.findByRole("button", { name: "Copy smart route Surge line for SOCKS5-TLS" });
        await waitFor(() => expect((tls as HTMLButtonElement).disabled).toBe(false));
        await userEvent.click(tls);
        expect(navigator.clipboard.writeText).not.toHaveBeenCalled();
        expect((await screen.findByRole("alert")).textContent).toBe("SOCKS5-TLS is not enabled any more. Reload the page and try again.");
    });
    it("hides SOCKS5 URLs once SOCKS5 is off", async () => {
        const { api } = await renderConsole();
        vi.mocked(api.nodeDetails).mockResolvedValueOnce({
            ...chengduDetails,
            profiles: [{ protocol: "snell", port: 10810, surge: "Chengdu 02 = snell, 127.0.0.1, 10810, psk=k, version=6" }],
        });
        await userEvent.click(screen.getByRole("button", { name: "KuaiFan" }));
        await userEvent.click((await screen.findAllByRole("button", { name: "Details for Chengdu 02" }))[0]);
        expect(await screen.findByRole("button", { name: "Copy Surge line for Snell v6" })).toBeTruthy();
        expect(screen.queryByRole("button", { name: "Copy SOCKS5 URL" })).toBeNull();
    });

    it("leaves the SOCKS5 URL out of the smart route without SOCKS5", async () => {
        const api = makeApi();
        vi.mocked(api.smartProxy).mockResolvedValue({ enabled: true, intervalMinutes: 30, running: false, targets: [], results: [] });
        render(<SmartProxyCard api={api} getSessionRevision={() => 0} onAccessLost={() => {}} protocols={["snell"]} />);
        expect(await screen.findByRole("button", { name: "Copy smart route Surge line for Snell v6" })).toBeTruthy();
        expect(screen.queryByRole("button", { name: "Copy smart route SOCKS5 URL" })).toBeNull();
    });
});

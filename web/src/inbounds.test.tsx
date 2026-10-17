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
        expect(within(rows[0]).getByText("Always on")).toBeTruthy();
        expect(within(rows[0]).queryByRole("switch")).toBeNull();
        expect(screen.queryByText(/Certificate fingerprint/)).toBeNull();

        await userEvent.click(switchFor("Snell v6"));
        expect(api.configureInbounds).toHaveBeenLastCalledWith({ snell: true, shadowsocks: false, socksTLS: false });
        await waitFor(() => expect(switchFor("Snell v6").checked).toBe(true));
        expect(within(rows[1]).getByText("Listening")).toBeTruthy();

        await userEvent.click(switchFor("SOCKS5-TLS"));
        expect(api.configureInbounds).toHaveBeenLastCalledWith({ snell: true, shadowsocks: false, socksTLS: true });
        expect(await screen.findByText(/Certificate fingerprint \(SHA-256\): 8bd0624728/)).toBeTruthy();
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
        const failing = inboundsStatus({ snell: true, shadowsocks: false, socksTLS: false });
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

        vi.mocked(api.configureInbounds).mockResolvedValueOnce(inboundsStatus({ snell: true, shadowsocks: true, socksTLS: false }));
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

    it("copies the smart route's Surge line for the chosen protocol", async () => {
        const api = makeApi();
        vi.mocked(api.smartProxy).mockResolvedValue({ enabled: true, intervalMinutes: 30, running: false, targets: [], results: [] });
        vi.mocked(api.smartProxyDetails).mockResolvedValue({
            url: "socks5://a:b@127.0.0.1:10808", socksAddress: "127.0.0.1:10808", socksUsername: "a", socksPassword: "b",
            profiles: [{ protocol: "snell", port: 10810, surge: "智能线路 = snell, 127.0.0.1, 10810, psk=s" }],
        });
        render(<SmartProxyCard api={api} getSessionRevision={() => 0} onAccessLost={() => {}} protocols={["socks5", "snell"]} />);
        const select = await screen.findByLabelText("Surge protocol");
        await waitFor(() => expect((select as HTMLSelectElement).disabled).toBe(false));
        await userEvent.selectOptions(select, "snell");
        await userEvent.click(screen.getByRole("button", { name: "Copy Surge line" }));
        expect(navigator.clipboard.writeText).toHaveBeenCalledWith("智能线路 = snell, 127.0.0.1, 10810, psk=s");
        expect(await screen.findByText("Smart route Surge line copied.")).toBeTruthy();
    });
});

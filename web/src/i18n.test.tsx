import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { App } from "./App";
import { getLocale, setLocale } from "./i18n";
import { makeApi, readyStatus } from "./test/fixtures";

describe("language switch", () => {
  it("switches the console between English and Chinese without writing browser storage", async () => {
    const storageWrite = vi.spyOn(Storage.prototype, "setItem");
    render(<App api={makeApi(readyStatus())} />);
    await screen.findByRole("heading", { name: "Service status" });

    const [switcher] = screen.getAllByRole("group", { name: "Language" });
    await userEvent.click(within(switcher).getByRole("button", { name: "中文" }));

    expect(await screen.findByRole("heading", { name: "服务状态" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "锁定控制台" })).toBeTruthy();
    expect(document.documentElement.lang).toBe("zh-CN");


    const [chineseSwitcher] = screen.getAllByRole("group", { name: "语言" });
    await userEvent.click(within(chineseSwitcher).getByRole("button", { name: "English" }));
    expect(await screen.findByRole("heading", { name: "Service status" })).toBeTruthy();
    expect(getLocale()).toBe("en");
    expect(storageWrite).not.toHaveBeenCalled();
  });

  it("translates service error messages into the selected language", async () => {
    setLocale("zh");
    const { describeError } = await import("./lib/errors");
    const { ApiError } = await import("./api");
    expect(describeError(new ApiError({ title: "Rejected", status: 401, code: "invalid_access_token" }))).toBe("访问令牌不正确。");
    expect(describeError(new Error("boom"))).toBe("本地服务未能完成此请求。");
  });
});

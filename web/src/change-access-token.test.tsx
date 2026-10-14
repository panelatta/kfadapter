import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { App } from "./App";
import { ApiError } from "./api";
import { currentConsumerURL, makeApi, updatedConsumerURL } from "./test/fixtures";

const current = "current-access-token-1";
const next = "replacement-token-12345";

async function openForm() {
  await screen.findByText(currentConsumerURL);
  await userEvent.click(screen.getByRole("button", { name: "Change access token" }));
  return screen.findByLabelText("Current access token");
}

async function fill(currentValue: string, nextValue: string, confirmation = nextValue) {
  await userEvent.type(screen.getByLabelText("Current access token"), currentValue);
  await userEvent.type(screen.getByLabelText("New access token"), nextValue);
  await userEvent.type(screen.getByLabelText("Confirm new access token"), confirmation);
  await userEvent.click(screen.getByRole("button", { name: "Save new token" }));
}

describe("changing the access token from the console", () => {
  it("submits the tokens, reloads the console and shows the new subscription URL", async () => {
    const api = makeApi();
    vi.mocked(api.changeAccessToken).mockImplementation(async () => {
      vi.mocked(api.subscriptionURL).mockResolvedValue({ url: updatedConsumerURL });
      return {};
    });
    render(<App api={api} />);
    await openForm();
    await fill(current, next);
    expect(api.changeAccessToken).toHaveBeenCalledWith(current, next);
    await screen.findByText(updatedConsumerURL);
    expect(screen.getByText(/Access token changed/)).toBeTruthy();
    expect(screen.queryByLabelText("Current access token")).toBeNull();
  });

  it("validates locally before calling the service", async () => {
    const api = makeApi();
    render(<App api={api} />);
    await openForm();
    await fill(current, next, "different-token-12345");
    expect((await screen.findByRole("alert")).textContent).toContain("do not match");
    await fill(current, current);
    expect((await screen.findByRole("alert")).textContent).toContain("differs from the current one");
    await fill(current, "short");
    expect((await screen.findByRole("alert")).textContent).toContain("16–128 bytes");
    expect(api.changeAccessToken).not.toHaveBeenCalled();
  });

  it("keeps the session when the current token is wrong", async () => {
    const api = makeApi();
    vi.mocked(api.changeAccessToken).mockRejectedValue(
      new ApiError({ type: "", code: "invalid_access_token", status: 401, title: "Authentication required" }),
    );
    render(<App api={api} />);
    await openForm();
    await fill(current, next);
    expect((await screen.findByRole("alert")).textContent).toContain("was not accepted");
    expect(screen.getByText(currentConsumerURL)).toBeTruthy();
    expect(screen.getByLabelText("Current access token")).toBeTruthy();
  });

  it("returns to the unlock screen when the session is gone", async () => {
    const api = makeApi();
    vi.mocked(api.changeAccessToken).mockRejectedValue(
      new ApiError({ type: "", code: "session_unavailable", status: 503, title: "Session unavailable" }),
    );
    render(<App api={api} />);
    await openForm();
    await fill(current, next);
    await waitFor(() => expect(screen.getByLabelText("Access token")).toBeTruthy());
    expect(screen.getByRole("alert").textContent).toContain("Unlock the console with the new access token");
  });
});

import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { navigation } from "@/api/client";
import { ME, mockApi } from "@/test/api";
import { renderApp } from "@/test/render";

async function openMenu() {
  await userEvent.click(await screen.findByRole("button", { name: /^Account:/ }));
  return screen.findByRole("menu");
}

describe("UserMenu", () => {
  it("shows the signed-in user's name, email and initials", async () => {
    mockApi();
    renderApp();
    const trigger = await screen.findByRole("button", { name: "Account: Alice Liddell" });
    expect(trigger).toHaveTextContent("AL");
    const menu = await openMenu();
    expect(menu).toHaveTextContent("Alice Liddell");
    expect(menu).toHaveTextContent("alice@example.test");
    // The CSP blocks <style>; the shared wrapper keeps Radix out of modal mode.
    expect(document.querySelectorAll("style")).toHaveLength(0);
  });

  it("falls back to the email when there is no display name", async () => {
    const me = { ...ME, display_name: "" };
    mockApi({ "/api/v1/me": me });
    renderApp("/inbox", me);
    expect(
      await screen.findByRole("button", { name: "Account: alice@example.test" }),
    ).toBeInTheDocument();
  });

  // 001-AC3: sign-out is a CSRF-carrying POST; the browser then follows the server's redirect
  // (the provider's end-session endpoint), so the provider session ends too.
  it("signs out with the CSRF token and follows the redirect", async () => {
    const go = vi.spyOn(navigation, "go").mockImplementation(() => {});
    const fn = mockApi({ "/auth/logout": { redirect: "https://idp.test/logout?id_token_hint=x" } });
    renderApp();
    await openMenu();
    await userEvent.click(screen.getByRole("menuitem", { name: "Sign out" }));
    await waitFor(() => expect(go).toHaveBeenCalledWith("https://idp.test/logout?id_token_hint=x"));
    const call = fn.mock.calls.find(([u]) => u === "/auth/logout");
    expect(call?.[1]?.method).toBe("POST");
    expect(new Headers(call?.[1]?.headers).get("X-CSRF-Token")).toBe(ME.csrf_token);
  });

  it("says when sign-out fails and stays signed in", async () => {
    const go = vi.spyOn(navigation, "go").mockImplementation(() => {});
    mockApi({ "/auth/logout": () => new Response("", { status: 500 }) });
    renderApp();
    await openMenu();
    await userEvent.click(screen.getByRole("menuitem", { name: "Sign out" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Sign-out failed");
    expect(go).not.toHaveBeenCalled();
  });

  // 001-AC2: dev mode has a single user and nothing to sign out of.
  it("explains dev mode instead of offering sign-out", async () => {
    const me = { ...ME, dev_mode: true };
    mockApi({ "/api/v1/me": me });
    renderApp("/inbox", me);
    const menu = await openMenu();
    expect(menu).toHaveTextContent("Dev mode");
    expect(screen.queryByRole("menuitem", { name: "Sign out" })).not.toBeInTheDocument();
  });
});

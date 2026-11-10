import { focusManager } from "@tanstack/react-query";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { MOBILE_QUERY } from "@/components/AppShell";
import { COLLAPSED_KEY, SIDEBAR_MAX, SIDEBAR_MIN, WIDTH_KEY } from "@/hooks/useSidebar";
import { ME, mockApi } from "@/test/api";
import { setMedia } from "@/test/media";
import { renderApp } from "@/test/render";

beforeEach(() => {
  mockApi();
});

const sidebar = () => screen.getByRole("complementary", { name: "Sidebar" });

describe("AppShell", () => {
  it("redirects / to the inbox and lists every section", async () => {
    renderApp("/");
    expect(await screen.findByRole("heading", { level: 1, name: "Inbox" })).toBeInTheDocument();
    const nav = screen.getByRole("navigation", { name: "Main" });
    expect(
      within(nav)
        .getAllByRole("link")
        .map((l) => l.textContent),
    ).toEqual([
      "Inbox",
      "Watches",
      "Channels",
      "Rules",
      "Scripts",
      "Audit",
      "API keys",
      "Settings",
    ]);
    expect(await screen.findByText("You are all caught up")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Set up a rule" })).toHaveAttribute("href", "/rules");
  });

  it("navigates and shows the section's empty state", async () => {
    renderApp();
    await userEvent.click(screen.getByRole("link", { name: "Rules" }));
    expect(screen.getByRole("heading", { level: 1, name: "Rules" })).toBeInTheDocument();
    expect(await screen.findByText("No rules yet")).toBeInTheDocument();
  });

  it("navigates to the watches list", async () => {
    mockApi({ "/api/v1/watches": { items: [] } });
    renderApp();
    await userEvent.click(screen.getByRole("link", { name: "Watches" }));
    expect(screen.getByRole("heading", { level: 1, name: "Watches" })).toBeInTheDocument();
    expect(await screen.findByText("No watches yet")).toBeInTheDocument();
  });

  it("shows a not-found page for unknown routes", () => {
    renderApp("/nowhere");
    expect(screen.getByRole("heading", { name: "Page not found" })).toBeInTheDocument();
  });

  it("shows the server version in the sidebar footer", async () => {
    renderApp();
    expect(screen.getByTestId("app-version")).toHaveTextContent("Loading version");
    expect(await screen.findByText("v1.2.3 (abc1234)")).toBeInTheDocument();
  });

  it("says when the version cannot be loaded", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("", { status: 500 })));
    renderApp();
    expect(await screen.findByText("Version unavailable")).toBeInTheDocument();
  });

  // 001-AC23: collapse to icons and expand back by button.
  it("collapses to icons and expands by button", async () => {
    renderApp();
    await userEvent.click(screen.getByRole("button", { name: "Collapse sidebar" }));
    expect(sidebar()).toHaveAttribute("data-collapsed", "true");
    expect(sidebar()).toHaveStyle({ width: "64px" });
    expect(screen.getByRole("link", { name: "Inbox" })).not.toHaveTextContent("Inbox");
    expect(localStorage.getItem(COLLAPSED_KEY)).toBe("true");

    await userEvent.click(screen.getByRole("button", { name: "Expand sidebar" }));
    expect(sidebar()).toHaveAttribute("data-collapsed", "false");
    expect(localStorage.getItem(COLLAPSED_KEY)).toBe("false");
  });

  // 001-AC23: Ctrl+B and Cmd+B toggle the sidebar.
  it.each([{ ctrlKey: true }, { metaKey: true }])("toggles with %o + B", (mod) => {
    renderApp();
    fireEvent.keyDown(window, { key: "b", ...mod });
    expect(sidebar()).toHaveAttribute("data-collapsed", "true");
    fireEvent.keyDown(window, { key: "B", ...mod });
    expect(sidebar()).toHaveAttribute("data-collapsed", "false");
    fireEvent.keyDown(window, { key: "b" });
    expect(sidebar()).toHaveAttribute("data-collapsed", "false");
  });

  // 001-AC23: drag resize is clamped to 180..480 and persisted.
  it.each([
    [1000, SIDEBAR_MAX],
    [-1000, SIDEBAR_MIN],
    [52, 300],
  ])("drag by %spx gives %spx", (dx, want) => {
    renderApp();
    const handle = screen.getByRole("separator", { name: "Resize sidebar" });
    fireEvent.pointerDown(handle, { clientX: 500, pointerId: 1 });
    fireEvent.pointerMove(handle, { clientX: 500 + dx, pointerId: 1 });
    fireEvent.pointerUp(handle, { pointerId: 1 });
    expect(sidebar()).toHaveStyle({ width: `${want}px` });
    expect(handle).toHaveAttribute("aria-valuenow", String(want));
    expect(localStorage.getItem(WIDTH_KEY)).toBe(String(want));
    // Moves after release are ignored.
    fireEvent.pointerMove(handle, { clientX: 0, pointerId: 1 });
    expect(sidebar()).toHaveStyle({ width: `${want}px` });
  });

  // 001-AC23: storage is written once per drag, at release, not on every move.
  it("does not write storage while dragging", () => {
    renderApp();
    const setItem = vi.spyOn(Storage.prototype, "setItem");
    const widthWrites = () => setItem.mock.calls.filter(([k]) => k === WIDTH_KEY);
    const handle = screen.getByRole("separator", { name: "Resize sidebar" });
    fireEvent.pointerDown(handle, { clientX: 500, pointerId: 1 });
    for (const x of [510, 540, 600, 650])
      fireEvent.pointerMove(handle, { clientX: x, pointerId: 1 });
    expect(widthWrites()).toHaveLength(0);
    fireEvent.pointerUp(handle, { pointerId: 1 });
    expect(widthWrites()).toEqual([[WIDTH_KEY, "398"]]);
    // A stray release without a drag writes nothing.
    fireEvent.pointerUp(handle, { pointerId: 1 });
    expect(widthWrites()).toHaveLength(1);
  });

  // 001-AC23: the shortcut leaves typing alone.
  it.each([
    ["input", () => document.createElement("input")],
    ["textarea", () => document.createElement("textarea")],
    ["select", () => document.createElement("select")],
    [
      "contenteditable child",
      () => {
        const host = document.createElement("div");
        host.setAttribute("contenteditable", "true");
        const child = document.createElement("span");
        host.append(child);
        return child;
      },
    ],
  ])("ignores Ctrl+B inside a %s", (_, make) => {
    renderApp();
    const el = make();
    document.body.append(el.closest("[contenteditable]") ?? el);
    fireEvent.keyDown(el, { key: "b", ctrlKey: true });
    expect(sidebar()).toHaveAttribute("data-collapsed", "false");
    (el.closest("[contenteditable]") ?? el).remove();
  });

  it("ignores Ctrl+B already handled by someone else", () => {
    renderApp();
    const el = document.createElement("div");
    document.body.append(el);
    el.addEventListener("keydown", (e) => e.preventDefault());
    fireEvent.keyDown(el, { key: "b", ctrlKey: true });
    expect(sidebar()).toHaveAttribute("data-collapsed", "false");
    el.remove();
  });

  // 001-AC23: keyboard resize uses the same clamps.
  it("resizes by keyboard", () => {
    renderApp();
    const handle = screen.getByRole("separator", { name: "Resize sidebar" });
    fireEvent.keyDown(handle, { key: "ArrowRight" });
    expect(handle).toHaveAttribute("aria-valuenow", "264");
    fireEvent.keyDown(handle, { key: "ArrowLeft" });
    fireEvent.keyDown(handle, { key: "ArrowLeft" });
    expect(handle).toHaveAttribute("aria-valuenow", "232");
    fireEvent.keyDown(handle, { key: "End" });
    expect(handle).toHaveAttribute("aria-valuenow", String(SIDEBAR_MAX));
    fireEvent.keyDown(handle, { key: "Home" });
    expect(handle).toHaveAttribute("aria-valuenow", String(SIDEBAR_MIN));
    fireEvent.keyDown(handle, { key: "a" });
    expect(handle).toHaveAttribute("aria-valuenow", String(SIDEBAR_MIN));
  });

  // 001-AC23: width and collapsed state survive a reload.
  it("restores width and collapsed state on load", () => {
    localStorage.setItem(WIDTH_KEY, "320");
    const first = renderApp();
    expect(sidebar()).toHaveStyle({ width: "320px" });
    first.unmount();

    localStorage.setItem(COLLAPSED_KEY, "true");
    renderApp();
    expect(sidebar()).toHaveAttribute("data-collapsed", "true");
    expect(screen.queryByRole("separator")).not.toBeInTheDocument();
  });
  // Act-as started or stopped in another tab, seen on focus: nothing typed or cached for the previous
  // account may stay, or a draft would be saved as the new one.
  it("drops the page and cached data when the effective user changes", async () => {
    const fn = mockApi({ "/api/v1/watches": { items: [] } });
    const { router, client } = renderApp("/rules/new");
    await userEvent.type(await screen.findByLabelText("Name"), "Alice draft");
    client.setQueryData(["sentinel"], "alice's");
    const after = mockApi({
      "/api/v1/watches": { items: [] },
      "/api/v1/me": {
        ...ME,
        id: "b",
        display_name: "Bob",
        acting_as: { id: ME.id, email: ME.email, display_name: ME.display_name },
      },
    });
    act(() => {
      focusManager.setFocused(false);
      focusManager.setFocused(true);
    });
    focusManager.setFocused(undefined);
    await waitFor(() => expect(router.state.location.pathname).toBe("/inbox"));
    expect(screen.queryByDisplayValue("Alice draft")).toBeNull();
    expect(client.getQueryData(["sentinel"])).toBeUndefined();
    expect(client.getQueryData(["me"])).toMatchObject({ id: "b" });
    const sent = [...fn.mock.calls, ...after.mock.calls].map(([, init]) =>
      String(init?.body ?? ""),
    );
    expect(sent.some((b) => b.includes("Alice draft"))).toBe(false);
  });
});

describe("AppShell on mobile", () => {
  beforeEach(() => setMedia(MOBILE_QUERY, true));

  it("turns the sidebar into a drawer", async () => {
    renderApp();
    expect(sidebar()).toHaveAttribute("inert");
    expect(screen.queryByRole("separator")).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Open navigation" }));
    expect(sidebar()).not.toHaveAttribute("inert");
    await userEvent.click(screen.getByTestId("drawer-backdrop"));
    expect(sidebar()).toHaveAttribute("inert");

    fireEvent.keyDown(window, { key: "b", ctrlKey: true });
    expect(screen.getByRole("button", { name: "Close navigation" })).toBeInTheDocument();
    fireEvent.keyDown(window, { key: "Escape" });
    expect(sidebar()).toHaveAttribute("inert");

    await userEvent.click(screen.getByRole("button", { name: "Open navigation" }));
    await userEvent.click(screen.getByRole("button", { name: "Close navigation" }));
    expect(sidebar()).toHaveAttribute("inert");
  });

  it("closes the drawer after navigating", async () => {
    renderApp();
    await userEvent.click(screen.getByRole("button", { name: "Open navigation" }));
    await userEvent.click(screen.getByRole("link", { name: "Rules" }));
    expect(screen.getByRole("heading", { level: 1, name: "Rules" })).toBeInTheDocument();
    expect(sidebar()).toHaveAttribute("inert");
  });

  it("closes the drawer when the viewport grows to desktop", async () => {
    renderApp();
    await userEvent.click(screen.getByRole("button", { name: "Open navigation" }));
    act(() => setMedia(MOBILE_QUERY, false));
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Collapse sidebar" })).toBeInTheDocument(),
    );
  });
});

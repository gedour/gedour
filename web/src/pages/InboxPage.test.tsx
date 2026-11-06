import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { mockApi } from "@/test/api";
import { setMedia } from "@/test/media";
import { renderApp } from "@/test/render";
import { FakeEventSource, inboxItem } from "@/test/routing";
import { lastCall, pending } from "@/test/watches";
import type { InboxItem } from "@/types/api";

const WIDE = "(min-width: 1024px)";

beforeEach(() => {
  setMedia(WIDE, true);
});
afterEach(() => {
  FakeEventSource.instances = [];
});

const yesterday = () => new Date(Date.now() - 24 * 3600 * 1000).toISOString();

/** A small inbox server: lists by state and applies bulk actions, so the UI's refetches see changes. */
function inboxServer(initial: InboxItem[]) {
  let items = initial;
  const unread = () => items.filter((i) => !i.read_at && !i.archived_at).length;
  const act = (patch: (i: InboxItem) => InboxItem) => (_: URL, init?: RequestInit) => {
    const { ids } = JSON.parse(String(init?.body)) as { ids: number[] };
    items = items.map((i) => (ids.includes(i.id) ? patch(i) : i));
    return Response.json({ updated: ids.length, unread: unread() });
  };
  const now = new Date().toISOString();
  const fn = mockApi({
    "/api/v1/inbox": (url: URL) => {
      const state = url.searchParams.get("state");
      const priority = url.searchParams.get("priority");
      const list = items.filter(
        (i) =>
          (state === "archived"
            ? i.archived_at
            : !i.archived_at && (state !== "unread" || !i.read_at)) &&
          (!priority || i.priority === priority),
      );
      return Response.json({ items: list });
    },
    "/api/v1/inbox/count": () => Response.json({ unread: unread() }),
    "/api/v1/inbox/read": act((i) => ({ ...i, read_at: now })),
    "/api/v1/inbox/unread": act((i) => ({ ...i, read_at: null })),
    "/api/v1/inbox/archive": act((i) => ({ ...i, archived_at: now })),
    "/api/v1/inbox/unarchive": act((i) => ({ ...i, archived_at: null })),
  });
  return { fn, items: () => items };
}

const rows = () => screen.getAllByRole("listitem").filter((li) => li.dataset.unread !== undefined);

describe("InboxPage", () => {
  // 001-AC20: grouped by day, unread emphasized, the sidebar badge shows the unread count.
  it("groups items by day and badges the unread count", async () => {
    inboxServer([
      inboxItem(3),
      inboxItem(2, { read_at: yesterday() }),
      inboxItem(1, { created_at: yesterday() }),
    ]);
    renderApp("/inbox");
    const today = await screen.findByRole("list", { name: "Inbox, Today" });
    expect(within(today).getAllByRole("listitem")).toHaveLength(2);
    expect(screen.getByRole("list", { name: "Inbox, Yesterday" })).toHaveTextContent(
      "v1.0.0 published",
    );
    expect(rows().map((r) => r.dataset.unread)).toEqual(["true", "false", "true"]);
    expect(within(today).getAllByText(/^Unread:/)).toHaveLength(1);
    expect(await screen.findByRole("link", { name: "Inbox, 2 unread" })).toBeInTheDocument();
    expect(screen.getByTestId("unread-badge")).toHaveTextContent("2");
  });

  // 001-AC24: loading, empty (per tab) and error states.
  it("has loading, empty and error states", async () => {
    mockApi({ "/api/v1/inbox": pending });
    const first = renderApp("/inbox");
    expect(screen.getAllByTestId("inbox-skeleton")).toHaveLength(5);
    first.unmount();

    inboxServer([]);
    const second = renderApp("/inbox");
    expect(await screen.findByRole("region", { name: "Nothing to read yet" })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Unread" }));
    expect(
      await screen.findByRole("region", { name: "You are all caught up" }),
    ).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Archived" }));
    expect(await screen.findByRole("region", { name: "Nothing archived" })).toBeInTheDocument();
    second.unmount();

    let fail = true;
    mockApi({
      "/api/v1/inbox": () =>
        fail ? new Response("", { status: 500 }) : Response.json({ items: [inboxItem(1)] }),
    });
    renderApp("/inbox");
    expect(await screen.findByRole("alert")).toHaveTextContent("The inbox could not be loaded.");
    fail = false;
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("v1.0.0 published")).toBeInTheDocument();
  });

  // 001-AC20: j/k move, u toggles read, e archives, x selects; ignored while typing.
  it("is driven by the keyboard", async () => {
    const srv = inboxServer([inboxItem(3), inboxItem(2), inboxItem(1)]);
    renderApp("/inbox");
    await screen.findByText("v3.0.0 published");
    const focused = () => rows().findIndex((r) => r.dataset.focused === "true");
    expect(focused()).toBe(0);
    await userEvent.keyboard("j");
    expect(focused()).toBe(1);
    expect(document.activeElement?.id).toBe("inbox-item-2");
    await userEvent.keyboard("jjj");
    expect(focused()).toBe(2);
    await userEvent.keyboard("k");
    expect(focused()).toBe(1);

    await userEvent.keyboard("u");
    await waitFor(() => expect(srv.items().find((i) => i.id === 2)?.read_at).not.toBeNull());
    await waitFor(() => expect(rows()[1]?.dataset.unread).toBe("false"));
    await userEvent.keyboard("u");
    await waitFor(() => expect(srv.items().find((i) => i.id === 2)?.read_at).toBeNull());

    await userEvent.keyboard("x");
    expect(screen.getByRole("checkbox", { name: "Select: v2.0.0 published" })).toBeChecked();
    expect(screen.getByRole("toolbar", { name: "Bulk actions" })).toHaveTextContent("1 selected");

    await userEvent.keyboard("e");
    await waitFor(() => expect(screen.queryByText("v2.0.0 published")).not.toBeInTheDocument());
    expect(srv.items().find((i) => i.id === 2)?.archived_at).not.toBeNull();
    // The archived item left the selection with the list.
    expect(screen.queryByRole("toolbar", { name: "Bulk actions" })).not.toBeInTheDocument();

    // Typing in a field never triggers a shortcut.
    const input = document.createElement("input");
    document.body.append(input);
    fireEvent.keyDown(input, { key: "e" });
    fireEvent.keyDown(window, { key: "j", ctrlKey: true });
    input.remove();
    expect(srv.items().filter((i) => i.archived_at)).toHaveLength(1);
    expect(screen.getByText("v3.0.0 published")).toBeInTheDocument();
  });

  // 001-AC20: select all (or some), then read or archive them in one call; only those ids are sent.
  it("bulk-reads and archives the selection", async () => {
    const srv = inboxServer([inboxItem(3), inboxItem(2), inboxItem(1)]);
    renderApp("/inbox");
    await screen.findByText("v3.0.0 published");
    const all = screen.getByRole("checkbox", { name: "Select all" });
    await userEvent.click(all);
    expect(screen.getByRole("toolbar")).toHaveTextContent("3 selected");
    await userEvent.click(screen.getByRole("checkbox", { name: "Select: v1.0.0 published" }));
    expect(all).toHaveProperty("indeterminate", true);
    await userEvent.click(screen.getByRole("button", { name: "Mark read" }));
    await waitFor(() =>
      expect(lastCall(srv.fn, "/api/v1/inbox/read", "POST")?.body).toEqual({ ids: [3, 2] }),
    );
    await waitFor(() =>
      expect(rows().map((r) => r.dataset.unread)).toEqual(["false", "false", "true"]),
    );
    expect(await screen.findByRole("link", { name: "Inbox, 1 unread" })).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Mark unread" }));
    await waitFor(() =>
      expect(rows().map((r) => r.dataset.unread)).toEqual(["true", "true", "true"]),
    );
    await userEvent.click(screen.getByRole("button", { name: "Archive" }));
    await waitFor(() => expect(screen.queryByText("v3.0.0 published")).not.toBeInTheDocument());
    expect(screen.getByText("v1.0.0 published")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Archived" }));
    await screen.findByText("v3.0.0 published");
    await userEvent.click(screen.getByRole("checkbox", { name: "Select all" }));
    await userEvent.click(screen.getByRole("button", { name: "Unarchive" }));
    await waitFor(() => expect(srv.items().every((i) => !i.archived_at)).toBe(true));
  });

  it("opens a detail panel that marks the item read", async () => {
    const srv = inboxServer([
      inboxItem(1, {
        event: {
          id: 1,
          type: "release.published",
          occurred_at: yesterday(),
          observed_at: yesterday(),
          payload: { tag: "v1.0.0", name: "One", url: "https://github.com/o/r/releases/v1.0.0" },
        },
      }),
    ]);
    renderApp("/inbox");
    await userEvent.click(await screen.findByRole("button", { name: /v1\.0\.0 published/ }));
    const panel = screen.getByRole("complementary", { name: "v1.0.0 published" });
    expect(within(panel).getByRole("link", { name: "Open upstream" })).toHaveAttribute(
      "href",
      "https://github.com/o/r/releases/v1.0.0",
    );
    expect(within(panel).getByRole("link", { name: "GitHub CLI releases" })).toHaveAttribute(
      "href",
      "/watches/w1",
    );
    expect(within(panel).getByRole("region", { name: "Details" })).toHaveTextContent("nameOne");
    await waitFor(() => expect(srv.items()[0]?.read_at).not.toBeNull());
    await userEvent.click(await within(panel).findByRole("button", { name: "Mark unread" }));
    await waitFor(() => expect(srv.items()[0]?.read_at).toBeNull());
    await userEvent.click(within(panel).getByRole("button", { name: "Archive" }));
    await waitFor(() => expect(srv.items()[0]?.archived_at).not.toBeNull());
    expect(
      screen.queryByRole("complementary", { name: "v1.0.0 published" }),
    ).not.toBeInTheDocument();
  });

  // 004-AC7: the ids as one row; the notes behind a closed disclosure, never as a row.
  it("shows the CVE ids and collapses the release notes", async () => {
    const notes = "## Security\nFixes CVE-2026-1234.\n" + "x".repeat(64 << 10);
    inboxServer([
      inboxItem(1, {
        event: {
          id: 1,
          type: "release.published",
          occurred_at: yesterday(),
          observed_at: yesterday(),
          payload: { tag: "v1.0.0", body: notes, cves: ["CVE-2026-1234", "CVE-2026-20871"] },
        },
      }),
    ]);
    renderApp("/inbox");
    await userEvent.click(await screen.findByRole("button", { name: /v1\.0\.0 published/ }));
    const panel = screen.getByRole("complementary", { name: "v1.0.0 published" });
    const details = within(panel).getByRole("region", { name: "Details" });
    expect(details).toHaveTextContent("cvesCVE-2026-1234, CVE-2026-20871");
    expect(details).not.toHaveTextContent("xxxxx");
    const disclosure = within(panel).getByText("Release notes").closest("details");
    expect(disclosure).not.toHaveAttribute("open");
    expect(disclosure).toHaveTextContent("Fixes CVE-2026-1234.");
    await userEvent.click(within(panel).getByText("Release notes"));
    expect(disclosure).toHaveAttribute("open");
  });

  // 004-AC4: nothing to show for a release without notes or ids.
  it("leaves out empty notes and an empty id list", async () => {
    inboxServer([
      inboxItem(1, {
        event: {
          id: 1,
          type: "release.published",
          occurred_at: yesterday(),
          observed_at: yesterday(),
          payload: { tag: "v1.0.0", body: "", cves: [] },
        },
      }),
    ]);
    renderApp("/inbox");
    await userEvent.click(await screen.findByRole("button", { name: /v1\.0\.0 published/ }));
    const panel = screen.getByRole("complementary", { name: "v1.0.0 published" });
    expect(within(panel).getByRole("region", { name: "Details" })).not.toHaveTextContent("cves");
    expect(within(panel).queryByText("Release notes")).not.toBeInTheDocument();
  });

  // 008-AC12: a new issue reads as one line, its labels as one row; an empty label list is left out.
  it("shows a new issue and its labels", async () => {
    const opened = (id: number, type: string, labels: string[]) =>
      inboxItem(id, {
        event: {
          id,
          type,
          occurred_at: yesterday(),
          observed_at: yesterday(),
          payload: {
            number: 12 + id,
            title: "Crash",
            author: "alice",
            labels,
            url: "https://github.com/o/r/issues/13",
          },
        },
      });
    inboxServer([opened(1, "issue.opened", ["bug", "ui"]), opened(2, "pr.opened", [])]);
    renderApp("/inbox");
    await userEvent.click(await screen.findByRole("button", { name: /Issue #13 opened by alice/ }));
    const panel = screen.getByRole("complementary", { name: "Issue #13 opened by alice" });
    expect(within(panel).getByRole("region", { name: "Details" })).toHaveTextContent(
      "labelsbug, ui",
    );
    await userEvent.click(screen.getByRole("button", { name: /PR #14 opened by alice/ }));
    const pr = screen.getByRole("complementary", { name: "PR #14 opened by alice" });
    expect(within(pr).getByRole("region", { name: "Details" })).not.toHaveTextContent("labels");
  });

  it("closes the panel with Escape or its button", async () => {
    inboxServer([inboxItem(1, { read_at: yesterday() })]);
    renderApp("/inbox");
    await userEvent.click(await screen.findByRole("button", { name: /v1\.0\.0 published/ }));
    await userEvent.click(screen.getByRole("button", { name: "Close details" }));
    expect(screen.queryByRole("button", { name: "Close details" })).not.toBeInTheDocument();
    expect(document.activeElement?.id).toBe("inbox-item-1");
    await userEvent.click(screen.getByRole("button", { name: /v1\.0\.0 published/ }));
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("button", { name: "Close details" })).not.toBeInTheDocument();
  });

  it("loads more pages", async () => {
    mockApi({
      "/api/v1/inbox": (url: URL) =>
        Response.json(
          url.searchParams.get("cursor")
            ? { items: [inboxItem(1)] }
            : { items: [inboxItem(3), inboxItem(2)], next_cursor: "2" },
        ),
    });
    renderApp("/inbox");
    await userEvent.click(await screen.findByRole("button", { name: "Load more" }));
    expect(await screen.findByText("v1.0.0 published")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Load more" })).not.toBeInTheDocument();
  });

  it("tells when an action fails", async () => {
    mockApi({
      "/api/v1/inbox": { items: [inboxItem(1)] },
      "/api/v1/inbox/read": () => new Response("", { status: 500 }),
    });
    renderApp("/inbox");
    await screen.findByText("v1.0.0 published");
    await userEvent.keyboard("u");
    expect(await screen.findByText(/Could not update the inbox/)).toBeInTheDocument();
  });

  // 001-AC19: a stream event updates the badge and the list live, with the arrival animation.
  it("updates live from the stream and refetches after a drop", async () => {
    vi.stubGlobal("EventSource", FakeEventSource);
    const srv = inboxServer([inboxItem(1)]);
    renderApp("/inbox");
    await screen.findByText("v1.0.0 published");
    const es = FakeEventSource.last();
    expect(es.url).toBe("/api/v1/inbox/stream");
    act(() => es.emit("inbox", { unread: 1 }));

    srv.items().unshift(inboxItem(2));
    act(() => es.emit("inbox", { unread: 2 }));
    const arrived = await screen.findByText("v2.0.0 published");
    expect(arrived.closest("li")).toHaveClass("animate-arrive");
    expect(screen.getByText("v1.0.0 published").closest("li")).not.toHaveClass("animate-arrive");
    expect(screen.getByRole("link", { name: "Inbox, 2 unread" })).toBeInTheDocument();

    srv.items().unshift(inboxItem(3));
    act(() => es.fail(false));
    expect(await screen.findByText("v3.0.0 published")).toBeInTheDocument();

    // 002-AC4: the stream's signed_out swaps the whole app for the sign-in gate.
    act(() => es.emit("signed_out", {}));
    expect(screen.getByRole("heading", { level: 1, name: "Sign in to continue" })).toBeVisible();
    expect(screen.queryByRole("complementary", { name: "Sidebar" })).not.toBeInTheDocument();
  });

  // 005-AC5: the badge says high in text, in the row and the side panel, and only on high items.
  it("badges high items in the list and the panel", async () => {
    inboxServer([inboxItem(2, { priority: "high" }), inboxItem(1)]);
    renderApp("/inbox");
    const high = await screen.findByRole("button", { name: /v2\.0\.0 published/ });
    expect(high).toHaveTextContent("High priority");
    expect(screen.getByRole("button", { name: /v1\.0\.0 published/ })).not.toHaveTextContent(
      "High priority",
    );
    await userEvent.click(high);
    expect(screen.getByRole("complementary", { name: "v2.0.0 published" })).toHaveTextContent(
      "High priority",
    );
    await userEvent.click(screen.getByRole("button", { name: /v1\.0\.0 published/ }));
    expect(screen.getByRole("complementary", { name: "v1.0.0 published" })).not.toHaveTextContent(
      "High priority",
    );
  });

  // 005-AC5: the detail dialog of narrow screens has the badge too.
  it("badges high items in the dialog", async () => {
    setMedia(WIDE, false);
    inboxServer([inboxItem(1, { priority: "high" })]);
    renderApp("/inbox");
    await userEvent.click(await screen.findByRole("button", { name: /v1\.0\.0 published/ }));
    expect(await screen.findByRole("dialog")).toHaveTextContent("High priority");
  });

  // 005-AC5: the filter asks for high items, lives in the URL with the state, and survives the tabs.
  it("filters on high priority", async () => {
    const srv = inboxServer([
      inboxItem(3, { priority: "high" }),
      inboxItem(2),
      inboxItem(1, { priority: "high", read_at: yesterday() }),
    ]);
    const { router } = renderApp("/inbox?state=unread");
    await screen.findByText("v3.0.0 published");
    const filter = screen.getByRole("button", { name: "High priority", pressed: false });
    await userEvent.click(filter);
    expect(filter).toHaveAttribute("aria-pressed", "true");
    await waitFor(() => expect(screen.queryByText("v2.0.0 published")).not.toBeInTheDocument());
    expect(new URLSearchParams(router.state.location.search).get("state")).toBe("unread");
    expect(new URLSearchParams(router.state.location.search).get("priority")).toBe("high");
    expect(lastCall(srv.fn, "/api/v1/inbox")?.url.searchParams.get("priority")).toBe("high");
    expect(screen.queryByText("v1.0.0 published")).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "All" }));
    expect(await screen.findByText("v1.0.0 published")).toBeInTheDocument();
    expect(router.state.location.search).toBe("?priority=high");
    expect(screen.queryByText("v2.0.0 published")).not.toBeInTheDocument();

    await userEvent.click(filter);
    expect(await screen.findByText("v2.0.0 published")).toBeInTheDocument();
    expect(router.state.location.search).toBe("");
    expect(lastCall(srv.fn, "/api/v1/inbox")?.url.searchParams.has("priority")).toBe(false);
  });

  // 005-AC5: a deep link applies the filter, and an unknown level does not.
  it("reads the filter from the URL", async () => {
    const srv = inboxServer([inboxItem(2, { priority: "high" }), inboxItem(1)]);
    const first = renderApp("/inbox?priority=high");
    expect(await screen.findByText("v2.0.0 published")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "High priority", pressed: true })).toBeVisible();
    expect(screen.queryByText("v1.0.0 published")).not.toBeInTheDocument();
    first.unmount();
    renderApp("/inbox?priority=urgent");
    expect(await screen.findByText("v1.0.0 published")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "High priority", pressed: false })).toBeVisible();
    expect(lastCall(srv.fn, "/api/v1/inbox")?.url.searchParams.has("priority")).toBe(false);
  });

  // 005-AC5: Load more stays within the filter, and the order is the server's, newest first.
  it("paginates within the filter", async () => {
    const fn = mockApi({
      "/api/v1/inbox": (url: URL) =>
        Response.json(
          url.searchParams.get("cursor") === "3"
            ? { items: [inboxItem(1, { priority: "high" })] }
            : {
                items: [inboxItem(5, { priority: "high" }), inboxItem(3, { priority: "high" })],
                next_cursor: "3",
              },
        ),
    });
    renderApp("/inbox?priority=high");
    await screen.findByText("v5.0.0 published");
    await userEvent.click(screen.getByRole("button", { name: "Load more" }));
    await screen.findByText("v1.0.0 published");
    const url = lastCall(fn, "/api/v1/inbox")?.url;
    expect(url?.searchParams.get("priority")).toBe("high");
    expect(url?.searchParams.get("cursor")).toBe("3");
    expect(rows().map((r) => within(r).getAllByRole("button")[0]?.textContent)).toEqual([
      expect.stringContaining("v5.0.0"),
      expect.stringContaining("v3.0.0"),
      expect.stringContaining("v1.0.0"),
    ]);
  });

  // Switching the filter shows a list, not arrivals: only new items animate.
  it("does not play the arrival animation when the filter changes", async () => {
    inboxServer([inboxItem(2, { priority: "high" }), inboxItem(1)]);
    renderApp("/inbox?priority=high");
    await screen.findByText("v2.0.0 published");
    await userEvent.click(screen.getByRole("button", { name: "High priority", pressed: true }));
    const shown = await screen.findByText("v1.0.0 published");
    expect(shown.closest("li")).not.toHaveClass("animate-arrive");
  });

  // 005-AC5: the empty filter says what is empty.
  it("has its own empty state", async () => {
    inboxServer([inboxItem(1)]);
    renderApp("/inbox?priority=high");
    expect(
      await screen.findByRole("region", { name: "Nothing high priority here" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "High priority", pressed: true })).toBeVisible();
  });

  // In the Unread tab, opening an item reads it and the refetch drops it: the panel must stay.
  it("keeps the opened item in the Unread tab", async () => {
    const srv = inboxServer([inboxItem(2), inboxItem(1)]);
    renderApp("/inbox?state=unread");
    await userEvent.click(await screen.findByRole("button", { name: /v2\.0\.0 published/ }));
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: /v2\.0\.0 published/ })).not.toBeInTheDocument(),
    );
    const panel = screen.getByRole("complementary", { name: "v2.0.0 published" });
    expect(srv.items().find((i) => i.id === 2)?.read_at).not.toBeNull();
    await userEvent.click(within(panel).getByRole("button", { name: "Mark unread" }));
    await waitFor(() => expect(srv.items().find((i) => i.id === 2)?.read_at).toBeNull());
    expect(within(panel).getByRole("button", { name: "Mark read" })).toBeInTheDocument();
    expect(await screen.findByRole("button", { name: /v2\.0\.0 published/ })).toBeInTheDocument();
  });

  // Below lg the detail is a modal dialog: focus moves into it and goes back to the row on close.
  it("shows the detail in a dialog on narrow screens", async () => {
    setMedia(WIDE, false);
    inboxServer([inboxItem(1, { read_at: yesterday() })]);
    renderApp("/inbox");
    const row = await screen.findByRole("button", { name: /v1\.0\.0 published/ });
    await userEvent.click(row);
    const dialog = screen.getByRole("dialog", { name: "v1.0.0 published" });
    expect(
      screen.queryByRole("complementary", { name: "v1.0.0 published" }),
    ).not.toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Close" })).toHaveFocus();
    // Shortcuts belong to the dialog while it is open.
    await userEvent.keyboard("e");
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    // What the browser does on Escape: the dialog closes itself.
    act(() => (dialog as HTMLDialogElement).close());
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(row).toHaveFocus();
  });

  it("keeps shortcuts on a checked checkbox and off menus and the drawer", async () => {
    const srv = inboxServer([inboxItem(2), inboxItem(1)]);
    renderApp("/inbox");
    await userEvent.click(
      await screen.findByRole("checkbox", { name: "Select: v2.0.0 published" }),
    );
    await userEvent.keyboard("j");
    expect(rows()[1]?.dataset.focused).toBe("true");
    for (const host of [
      Object.assign(document.createElement("div"), { role: "menu" }),
      document.getElementById("app-sidebar"),
    ]) {
      const btn = document.createElement("button");
      if (host?.getAttribute("role") === "menu") host.setAttribute("role", "menu");
      (host as HTMLElement).append(btn);
      if (!host?.isConnected) document.body.append(host as HTMLElement);
      fireEvent.keyDown(btn, { key: "e" });
      btn.remove();
    }
    expect(srv.items().every((i) => !i.archived_at)).toBe(true);
  });

  it("disables actions while one is pending", async () => {
    let release: (r: Response) => void = () => {};
    mockApi({
      "/api/v1/inbox": { items: [inboxItem(2), inboxItem(1)] },
      "/api/v1/inbox/read": () => new Promise<Response>((r) => (release = r)),
    });
    renderApp("/inbox");
    await userEvent.click(await screen.findByRole("checkbox", { name: "Select all" }));
    await userEvent.click(screen.getByRole("button", { name: "Mark read" }));
    expect(screen.getByRole("button", { name: "Archive" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Mark unread" })).toBeDisabled();
    act(() => release(Response.json({ updated: 2, unread: 0 })));
    await waitFor(() => expect(screen.getByRole("button", { name: "Archive" })).toBeEnabled());
  });
});

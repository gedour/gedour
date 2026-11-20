import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { mockApi } from "@/test/api";
import { setMedia } from "@/test/media";
import { renderApp } from "@/test/render";
import { FakeEventSource, inboxItem, rule } from "@/test/routing";
import { lastCall, pending, watch } from "@/test/watches";
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
function inboxServer(initial: InboxItem[], extra: Parameters<typeof mockApi>[0] = {}) {
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
      const watch = url.searchParams.get("watch");
      const list = items.filter(
        (i) =>
          (state === "archived"
            ? i.archived_at
            : !i.archived_at && (state !== "unread" || !i.read_at)) &&
          (!priority || i.priority === priority) &&
          (!watch || i.watch.id === watch),
      );
      return Response.json({ items: list });
    },
    "/api/v1/inbox/count": () =>
      Response.json({
        unread: unread(),
        read: items.filter((i) => i.read_at && !i.archived_at).length,
      }),
    "/api/v1/inbox/read": act((i) => ({ ...i, read_at: now })),
    "/api/v1/inbox/unread": act((i) => ({ ...i, read_at: null })),
    "/api/v1/inbox/archive": act((i) => ({ ...i, archived_at: now })),
    "/api/v1/inbox/unarchive": act((i) => ({ ...i, archived_at: null })),
    ...extra,
  });
  return { fn, items: () => items };
}

// The page sends only UUIDs as a watch filter.
const W2 = "22222222-2222-4222-8222-222222222222";
const GO = { id: W2, name: "Go releases", kind: "github_repo" as const };
const watchRoute = { [`/api/v1/watches/${W2}`]: watch({ id: W2 }) };

const rows = () => screen.getAllByRole("listitem").filter((li) => li.dataset.unread !== undefined);

describe("InboxPage", () => {
  // 001-AC20: grouped by day, unread emphasized, the sidebar badge shows the unread count.
  it("groups items by day and badges the unread count", async () => {
    inboxServer([
      inboxItem(3),
      inboxItem(2, { read_at: yesterday() }),
      inboxItem(1, { created_at: yesterday() }),
    ]);
    renderApp("/inbox?state=all");
    const today = await screen.findByRole("list", { name: "Inbox, Today" });
    expect(within(today).getAllByRole("listitem")).toHaveLength(2);
    expect(screen.getByRole("list", { name: "Inbox, Yesterday" })).toHaveTextContent(
      "v1.0.0 published",
    );
    expect(rows().map((r) => r.dataset.unread)).toEqual(["true", "false", "true"]);
    expect(within(today).getAllByText(/^Unread:/)).toHaveLength(1);
    // The watch leads the row, the event follows.
    expect(
      within(today).getByRole("button", {
        name: /^Unread:\s*GitHub CLI releases\s*v3\.0\.0 published/,
      }),
    ).toBeInTheDocument();
    expect(
      within(today).getByRole("button", { name: /^GitHub CLI releases\s*v2\.0\.0 published/ }),
    ).toBeInTheDocument();
    expect(await screen.findByRole("link", { name: "Inbox, 2 unread" })).toBeInTheDocument();
    expect(screen.getByTestId("unread-badge")).toHaveTextContent("2");
  });

  it("offers a rule only while none exists, and counts read items quietly", async () => {
    inboxServer([inboxItem(2, { read_at: yesterday() }), inboxItem(1, { read_at: yesterday() })], {
      "/api/v1/rules": { items: [rule()] },
    });
    renderApp("/inbox");
    const empty = await screen.findByRole("region", { name: "You are all caught up" });
    expect(await within(empty).findByText(/2 read/)).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Set up a rule" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "All", description: "2 read" })).toHaveTextContent(
      "2",
    );
    await userEvent.click(within(empty).getByRole("button", { name: "View all" }));
    expect(await screen.findByText("v2.0.0 published")).toBeInTheDocument();
  });

  // 001-AC24: loading, empty (per tab) and error states.
  it("has loading, empty and error states", async () => {
    mockApi({ "/api/v1/inbox": pending });
    const first = renderApp("/inbox?state=all");
    expect(screen.getAllByTestId("inbox-skeleton")).toHaveLength(5);
    first.unmount();

    inboxServer([]);
    const second = renderApp("/inbox?state=all");
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
    renderApp("/inbox?state=all");
    expect(await screen.findByRole("alert")).toHaveTextContent("The inbox could not be loaded.");
    fail = false;
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("v1.0.0 published")).toBeInTheDocument();
  });

  // 001-AC20: j/k move, u toggles read, e archives, x selects; ignored while typing.
  it("is driven by the keyboard", async () => {
    const srv = inboxServer([inboxItem(3), inboxItem(2), inboxItem(1)]);
    renderApp("/inbox?state=all");
    await screen.findByText("v3.0.0 published");
    const focused = () => rows().findIndex((r) => r.dataset.focused === "true");
    // No cursor until one is asked for: a shortcut never acts on a row nobody picked.
    expect(focused()).toBe(-1);
    await userEvent.keyboard("e");
    expect(srv.items().every((i) => !i.archived_at)).toBe(true);
    await userEvent.keyboard("j");
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
    expect(
      screen.getByRole("checkbox", { name: "Select: GitHub CLI releases: v2.0.0 published" }),
    ).toBeChecked();
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
    renderApp("/inbox?state=all");
    await screen.findByText("v3.0.0 published");
    await userEvent.click(
      screen.getByRole("checkbox", { name: "Select: GitHub CLI releases: v3.0.0 published" }),
    );
    const bar = screen.getByRole("toolbar", { name: "Bulk actions" });
    await userEvent.click(within(bar).getByRole("button", { name: "Select all 3" }));
    expect(bar).toHaveTextContent("3 selected");
    await userEvent.click(
      screen.getByRole("checkbox", { name: "Select: GitHub CLI releases: v1.0.0 published" }),
    );
    expect(within(bar).getByRole("button", { name: "Select all 3" })).toBeInTheDocument();
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
    await userEvent.click(
      screen.getByRole("checkbox", { name: "Select: GitHub CLI releases: v3.0.0 published" }),
    );
    await userEvent.click(screen.getByRole("button", { name: /^Select all/ }));
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
    expect(within(panel).getByRole("button", { name: "Unarchive" })).toBeInTheDocument();
    await userEvent.click(within(panel).getByRole("button", { name: "Close details" }));
    await waitFor(() => expect(rows()).toHaveLength(0));
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
    renderApp("/inbox?state=all");
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
    await userEvent.keyboard("ju");
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
    expect(router.state.location.search).toBe("?priority=high");
    expect(lastCall(srv.fn, "/api/v1/inbox")?.url.searchParams.get("priority")).toBe("high");
    expect(screen.queryByText("v1.0.0 published")).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "All" }));
    expect(await screen.findByText("v1.0.0 published")).toBeInTheDocument();
    expect(router.state.location.search).toBe("?state=all&priority=high");
    expect(screen.queryByText("v2.0.0 published")).not.toBeInTheDocument();

    await userEvent.click(filter);
    expect(await screen.findByText("v2.0.0 published")).toBeInTheDocument();
    expect(router.state.location.search).toBe("?state=all");

    await userEvent.click(screen.getByRole("button", { name: /^Unread/, pressed: false }));
    await waitFor(() => expect(router.state.location.search).toBe(""));
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

  // 010-AC13: a notification's link opens its event's item and leaves the filter in the URL.
  it("opens the item a notification links to", async () => {
    const srv = inboxServer([inboxItem(2), inboxItem(1, { priority: "high" })]);
    const { router, unmount } = renderApp("/inbox?priority=high&event=1");
    expect(
      await screen.findByRole("complementary", { name: "v1.0.0 published" }),
    ).toBeInTheDocument();
    await waitFor(() => expect(srv.items()[1]?.read_at).not.toBeNull());
    expect(router.state.location.search).toBe("?priority=high&state=all");
    unmount();
    const missing = renderApp("/inbox?event=99");
    expect(await screen.findByText("v2.0.0 published")).toBeInTheDocument();
    await waitFor(() => expect(missing.router.state.location.search).toBe("?state=all"));
    expect(screen.queryByRole("complementary", { name: /published/ })).not.toBeInTheDocument();
    // The link was not dropped silently.
    expect(
      await screen.findByText(
        "That event is not among the inbox items loaded here: it may be archived or older.",
      ),
    ).toBeInTheDocument();
  });

  // The home is the Unread tab, but a link to an item already read still opens it, on the All tab.
  it("opens a linked item already read", async () => {
    inboxServer([inboxItem(2), inboxItem(1, { read_at: yesterday() })]);
    const { router } = renderApp("/inbox?event=1");
    expect(
      await screen.findByRole("complementary", { name: "v1.0.0 published" }),
    ).toBeInTheDocument();
    await waitFor(() => expect(router.state.location.search).toBe("?state=all"));
    expect(screen.getByRole("button", { name: "All", pressed: true })).toBeVisible();
  });

  // 010-AC13: a cached list from an earlier visit lacks a newly pushed event: the link waits for the refetch.
  it("resolves a link against the refetched list, not the cache", async () => {
    let items = [inboxItem(1)];
    mockApi({ "/api/v1/inbox": () => Response.json({ items }) });
    const { router } = renderApp("/inbox");
    expect(await screen.findByText("v1.0.0 published")).toBeInTheDocument();
    await act(() => router.navigate("/channels"));
    items = [inboxItem(2), inboxItem(1)];
    await act(() => router.navigate("/inbox?event=2"));
    expect(
      await screen.findByRole("complementary", { name: "v2.0.0 published" }),
    ).toBeInTheDocument();
    await waitFor(() => expect(router.state.location.search).toBe("?state=all"));
    expect(screen.queryByText(/not among the inbox items/)).not.toBeInTheDocument();
  });

  // 010-AC13: a failed refetch still resolves the link: a miss warns and leaves the URL.
  it("drops a link when the inbox cannot be loaded", async () => {
    let fail = false;
    mockApi({
      "/api/v1/inbox": () =>
        fail ? new Response("", { status: 500 }) : Response.json({ items: [inboxItem(1)] }),
    });
    const { router } = renderApp("/inbox");
    expect(await screen.findByText("v1.0.0 published")).toBeInTheDocument();
    await act(() => router.navigate("/channels"));
    fail = true;
    await act(() => router.navigate("/inbox?event=7"));
    await waitFor(() => expect(router.state.location.search).toBe("?state=all"));
    expect(await screen.findByText(/not among the inbox items/)).toBeInTheDocument();
  });

  // 010-AC13: a link followed while an action is pending still marks its item read; the same link twice opens it twice.
  it("marks a linked item read while another action is pending", async () => {
    let release: (r: Response) => void = () => {};
    const archive = vi.fn(() => new Promise<Response>((r) => (release = r)));
    const fn = mockApi({
      "/api/v1/inbox": { items: [inboxItem(2), inboxItem(1)] },
      "/api/v1/inbox/archive": archive,
      "/api/v1/inbox/read": () => Response.json({ updated: 1, unread: 1 }),
    });
    const { router } = renderApp("/inbox");
    expect(await screen.findByText("v2.0.0 published")).toBeInTheDocument();
    await userEvent.keyboard("je");
    await waitFor(() => expect(archive).toHaveBeenCalled());
    await act(() => router.navigate("/inbox?event=1"));
    expect(
      await screen.findByRole("complementary", { name: "v1.0.0 published" }),
    ).toBeInTheDocument();
    await waitFor(() =>
      expect(lastCall(fn, "/api/v1/inbox/read", "POST")?.body).toEqual({ ids: [1] }),
    );
    await waitFor(() => expect(router.state.location.search).toBe("?state=all"));
    release(Response.json({ updated: 1, unread: 1 }));
    await userEvent.click(screen.getByRole("button", { name: "Close details" }));
    await act(() => router.navigate("/inbox?event=1"));
    expect(
      await screen.findByRole("complementary", { name: "v1.0.0 published" }),
    ).toBeInTheDocument();
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
    await userEvent.click(await screen.findByRole("button", { name: /v1\.0\.0 published/ }));
    await waitFor(() => expect(srv.items().find((i) => i.id === 1)?.read_at).not.toBeNull());
    // Read, refetched without it, the oldest row still holds its place while its panel shows.
    await waitFor(() => expect(lastCall(srv.fn, "/api/v1/inbox", "GET")).toBeDefined());
    expect(rows().map((r) => r.dataset.unread)).toEqual(["true", "false"]);
    await userEvent.click(screen.getByRole("button", { name: /v2\.0\.0 published/ }));
    await waitFor(() => expect(srv.items().find((i) => i.id === 2)?.read_at).not.toBeNull());
    // Another item opened: the first one leaves.
    await waitFor(() => expect(rows()).toHaveLength(1));
    const panel = screen.getByRole("complementary", { name: "v2.0.0 published" });
    expect(rows()[0]?.dataset.unread).toBe("false");
    await userEvent.click(within(panel).getByRole("button", { name: "Mark unread" }));
    await waitFor(() => expect(srv.items().find((i) => i.id === 2)?.read_at).toBeNull());
    expect(within(panel).getByRole("button", { name: "Mark read" })).toBeInTheDocument();
    expect(rows()[0]?.dataset.unread).toBe("true");
    await userEvent.click(screen.getByRole("button", { name: "Close details" }));
    expect(await screen.findByRole("button", { name: /v2\.0\.0 published/ })).toBeInTheDocument();
    expect(screen.queryByText("v1.0.0 published")).not.toBeInTheDocument();
  });

  // Below lg the detail is a modal dialog: focus moves into it and goes back to the row on close.
  it("shows the detail in a dialog on narrow screens", async () => {
    setMedia(WIDE, false);
    inboxServer([inboxItem(1, { read_at: yesterday() })]);
    renderApp("/inbox?state=all");
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
      await screen.findByRole("checkbox", {
        name: "Select: GitHub CLI releases: v2.0.0 published",
      }),
    );
    await userEvent.keyboard("jj");
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

  // The row's own actions: in the Unread tab a read row leaves at once, so the next one comes under
  // the same button, even before the server answers.
  it("repeats a row action in place", async () => {
    const releases: ((r: Response) => void)[] = [];
    const read = vi.fn(() => new Promise<Response>((r) => releases.push(r)));
    mockApi({
      "/api/v1/inbox": { items: [inboxItem(3), inboxItem(2), inboxItem(1)] },
      "/api/v1/inbox/read": read,
    });
    renderApp("/inbox");
    const quick = (text: string, name: string) =>
      within(
        screen.getByRole("group", { name: `Actions: GitHub CLI releases: ${text}` }),
      ).getByRole("button", { name });
    await screen.findByText("v3.0.0 published");
    await userEvent.click(quick("v3.0.0 published", "Mark as read"));
    await userEvent.click(quick("v2.0.0 published", "Mark as read"));
    expect(rows()).toHaveLength(1);
    expect(read).toHaveBeenCalledTimes(2);
    for (const r of releases) act(() => r(Response.json({ updated: 1, unread: 1 })));
    await waitFor(() => expect(rows()).toHaveLength(3));
  });

  // Even while another action is still out.
  it("brings a row back when its action fails", async () => {
    mockApi({
      "/api/v1/inbox": { items: [inboxItem(2), inboxItem(1)] },
      "/api/v1/inbox/read": pending,
      "/api/v1/inbox/archive": () => new Response(null, { status: 500 }),
    });
    renderApp("/inbox?state=all");
    await screen.findByText("v2.0.0 published");
    const quick = (text: string, name: string) =>
      within(
        screen.getByRole("group", { name: `Actions: GitHub CLI releases: ${text}` }),
      ).getByRole("button", { name });
    await userEvent.click(quick("v1.0.0 published", "Mark as read"));
    await userEvent.click(quick("v2.0.0 published", "Archive item"));
    expect(await screen.findByText(/Could not update the inbox/)).toBeInTheDocument();
    expect(rows()).toHaveLength(2);
  });

  describe("watch filter", () => {
    // Two watches, newest first: 4 and 2 from Go releases, 3 and 1 from GitHub CLI releases.
    const mixed = () => [
      inboxItem(4, { watch: GO, priority: "high" }),
      inboxItem(3, { priority: "high" }),
      inboxItem(2, { watch: GO, priority: "high" }),
      inboxItem(1, { priority: "high" }),
    ];

    // 020-AC5, 020-AC6, 020-AC7: the watch name filters in the same tab and priority, opens nothing,
    // the chip removes it, and history steps back into it.
    it("filters by the watch name and removes the filter with the chip", async () => {
      const srv = inboxServer(mixed(), watchRoute);
      const { router } = renderApp("/inbox?state=all&priority=high");
      await screen.findByText("v3.0.0 published");
      const reads = srv.fn.mock.calls.length;
      await userEvent.click(
        screen.getAllByRole("link", { name: "Show only Go releases" })[0] as HTMLElement,
      );
      await waitFor(() => expect(screen.queryByText("v3.0.0 published")).not.toBeInTheDocument());
      expect(router.state.location.search).toBe(`?state=all&priority=high&watch=${W2}`);
      const url = lastCall(srv.fn, "/api/v1/inbox")?.url;
      expect(url?.searchParams.get("watch")).toBe(W2);
      expect(url?.searchParams.get("priority")).toBe("high");
      expect(url?.searchParams.get("state")).toBe("all");
      expect(rows()).toHaveLength(2);
      expect(screen.getByRole("button", { name: "All", pressed: true })).toBeVisible();
      expect(screen.getByRole("button", { name: "High priority", pressed: true })).toBeVisible();
      // Not opened: no panel, no read sent.
      expect(screen.queryByRole("complementary", { name: /published/ })).not.toBeInTheDocument();
      expect(
        srv.fn.mock.calls
          .slice(reads)
          .some(([input]) => String(input).includes("/api/v1/inbox/read")),
      ).toBe(false);
      expect(srv.items().every((i) => !i.read_at)).toBe(true);

      await userEvent.click(screen.getByRole("button", { name: "Remove filter Go releases" }));
      expect(await screen.findByText("v3.0.0 published")).toBeInTheDocument();
      expect(router.state.location.search).toBe("?state=all&priority=high");
      expect(lastCall(srv.fn, "/api/v1/inbox")?.url.searchParams.has("watch")).toBe(false);
      expect(screen.queryByRole("button", { name: /^Remove filter/ })).not.toBeInTheDocument();
      expect(rows()).toHaveLength(4);

      await act(() => router.navigate(-1));
      await waitFor(() => expect(rows()).toHaveLength(2));
      expect(router.state.location.search).toBe(`?state=all&priority=high&watch=${W2}`);
      expect(screen.getByRole("button", { name: "Remove filter Go releases" })).toBeVisible();
    });

    // 020-AC10: a real link, by Tab, named for its watch, never inside the row's open button, whose
    // name still carries the unread marker, the watch and the summary.
    it("makes the watch name a link outside the open button", async () => {
      inboxServer(mixed());
      renderApp("/inbox?state=all");
      await screen.findByText("v4.0.0 published");
      const row = rows()[0] as HTMLElement;
      const link = within(row).getByRole("link", { name: "Show only Go releases" });
      expect(link).toHaveAttribute("href", `/inbox?state=all&watch=${W2}`);
      expect(link).toHaveTextContent(/^Go releases$/);
      expect(document.querySelectorAll("button a, a button")).toHaveLength(0);
      const open = within(row).getByRole("button", {
        name: /^Unread:\s*Go releases\s*v4\.0\.0 published/,
      });
      // The time sits outside the button: it comes back as the button's description.
      expect(open).toHaveAccessibleDescription("just now");
      within(row).getByRole("checkbox").focus();
      await userEvent.tab();
      expect(link).toHaveFocus();
      await userEvent.tab();
      expect(document.activeElement?.id).toBe("inbox-item-4");
    });

    // 020-AC7: a reload lands on the filter; the chip names the watch with no item loaded, from the
    // watch itself. 020-AC9: the empty state names it and removes the filter.
    it("names the watch with no item loaded and offers to remove the filter", async () => {
      const srv = inboxServer([inboxItem(2, { watch: GO, read_at: yesterday() }), inboxItem(1)], {
        ...watchRoute,
      });
      const { router } = renderApp(`/inbox?watch=${W2}`);
      const empty = await screen.findByRole("region", { name: "Nothing from Go releases here" });
      expect(screen.getByRole("button", { name: "Remove filter Go releases" })).toBeVisible();
      expect(lastCall(srv.fn, `/api/v1/watches/${W2}`)).toBeDefined();
      await userEvent.click(within(empty).getByRole("button", { name: "Show all watches" }));
      expect(await screen.findByText("v1.0.0 published")).toBeInTheDocument();
      expect(router.state.location.search).toBe("");
    });

    // 020-AC6, 020-AC7: a watch that cannot be read gets a neutral name, and the chip still goes.
    it("keeps an unknown watch removable", async () => {
      inboxServer([inboxItem(1)]);
      const { router } = renderApp("/inbox?state=all&watch=99999999-9999-4999-8999-999999999999");
      expect(
        await screen.findByRole("region", { name: "Nothing from Unknown watch here" }),
      ).toBeInTheDocument();
      await userEvent.click(screen.getByRole("button", { name: "Remove filter Unknown watch" }));
      expect(await screen.findByText("v1.0.0 published")).toBeInTheDocument();
      expect(router.state.location.search).toBe("?state=all");
    });

    // 020-AC8: select all then Mark read sends only the loaded items of the watch, and the refetch
    // after it keeps the filter.
    it("marks read only the filtered watch's items", async () => {
      const srv = inboxServer(mixed(), watchRoute);
      renderApp(`/inbox?watch=${W2}`);
      await screen.findByText("v4.0.0 published");
      await userEvent.click(
        screen.getByRole("checkbox", { name: "Select: Go releases: v4.0.0 published" }),
      );
      await userEvent.click(screen.getByRole("button", { name: "Select all 2" }));
      await userEvent.click(screen.getByRole("button", { name: "Mark read" }));
      await waitFor(() =>
        expect(lastCall(srv.fn, "/api/v1/inbox/read", "POST")?.body).toEqual({ ids: [4, 2] }),
      );
      expect(
        srv
          .items()
          .filter((i) => i.read_at)
          .map((i) => i.id),
      ).toEqual([4, 2]);
      expect(
        await screen.findByRole("region", { name: "Nothing from Go releases here" }),
      ).toBeInTheDocument();
      expect(lastCall(srv.fn, "/api/v1/inbox")?.url.searchParams.get("watch")).toBe(W2);
      // Named from the watch once no item is left to name it.
      expect(screen.getByRole("button", { name: "Remove filter Go releases" })).toBeVisible();
    });

    // 020-AC5: Load more stays within the watch.
    it("paginates within the watch", async () => {
      const fn = mockApi({
        "/api/v1/inbox": (url: URL) =>
          Response.json(
            url.searchParams.get("cursor")
              ? { items: [inboxItem(2, { watch: GO })] }
              : { items: [inboxItem(4, { watch: GO })], next_cursor: "4" },
          ),
      });
      renderApp(`/inbox?watch=${W2}`);
      await userEvent.click(await screen.findByRole("button", { name: "Load more" }));
      await screen.findByText("v2.0.0 published");
      const url = lastCall(fn, "/api/v1/inbox")?.url;
      expect(url?.searchParams.get("cursor")).toBe("4");
      expect(url?.searchParams.get("watch")).toBe(W2);
    });

    // 020-AC8: history into a filter drops what the previous view held: an item of another watch opened
    // there, a selection, the j/k cursor. Nothing the reader did not pick here joins an action.
    it("starts a view reached by history afresh", async () => {
      const srv = inboxServer(mixed(), watchRoute);
      const { router } = renderApp("/inbox?state=all");
      await screen.findByText("v3.0.0 published");
      await act(() => router.navigate(`/inbox?state=all&watch=${W2}`));
      await waitFor(() => expect(rows()).toHaveLength(2));
      await act(() => router.navigate(-1));
      const v4 = () =>
        screen.getByRole("checkbox", { name: "Select: Go releases: v4.0.0 published" });
      await userEvent.click(await screen.findByRole("button", { name: /v3\.0\.0 published/ }));
      expect(screen.getByRole("complementary", { name: "v3.0.0 published" })).toBeVisible();
      // A row the next view also lists, selected; the cursor on its last row.
      await userEvent.click(v4());
      await userEvent.keyboard("jj");
      expect(rows()[3]?.dataset.focused).toBe("true");
      await act(() => router.navigate(1));
      await waitFor(() => expect(screen.queryByText("v3.0.0 published")).not.toBeInTheDocument());
      expect(screen.queryByRole("complementary", { name: /published/ })).not.toBeInTheDocument();
      expect(v4()).not.toBeChecked();
      expect(screen.queryByRole("toolbar", { name: "Bulk actions" })).not.toBeInTheDocument();
      expect(rows().map((r) => r.dataset.focused)).toEqual(["false", "false"]);
      await userEvent.keyboard("e");
      await act(async () => {});
      expect(lastCall(srv.fn, "/api/v1/inbox/archive", "POST")).toBeUndefined();
      await userEvent.keyboard("j");
      expect(rows().map((r) => r.dataset.focused)).toEqual(["true", "false"]);
      await userEvent.click(
        screen.getByRole("checkbox", { name: "Select: Go releases: v4.0.0 published" }),
      );
      await userEvent.click(screen.getByRole("button", { name: "Select all 2" }));
      await userEvent.click(screen.getByRole("button", { name: "Archive" }));
      await waitFor(() =>
        expect(lastCall(srv.fn, "/api/v1/inbox/archive", "POST")?.body).toEqual({ ids: [4, 2] }),
      );
    });

    // 020-AC7, 020-AC9: while the watch is still being asked for, nothing reads as an empty name.
    it("names the filter neutrally until the watch answers", async () => {
      let release: (r: Response) => void = () => {};
      inboxServer([], {
        [`/api/v1/watches/${W2}`]: () => new Promise<Response>((r) => (release = r)),
      });
      renderApp(`/inbox?watch=${W2}`);
      expect(
        await screen.findByRole("region", { name: "Nothing from this watch here" }),
      ).toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Remove watch filter" })).toBeVisible();
      expect(screen.getByText("Watch filter")).toBeVisible();
      act(() => release(Response.json(watch({ id: W2 }))));
      expect(
        await screen.findByRole("region", { name: "Nothing from Go releases here" }),
      ).toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Remove filter Go releases" })).toBeVisible();
    });

    // 020-AC6, 020-AC7: a hand-edited id that is not a UUID is an unknown watch, never sent.
    it("never sends a watch that is not a UUID", async () => {
      const srv = inboxServer([inboxItem(1)]);
      const { router } = renderApp("/inbox?state=all&watch=nope");
      expect(
        await screen.findByRole("region", { name: "Nothing from Unknown watch here" }),
      ).toBeInTheDocument();
      expect(lastCall(srv.fn, "/api/v1/inbox")).toBeUndefined();
      expect(srv.fn.mock.calls.some(([input]) => String(input).includes("nope"))).toBe(false);
      await userEvent.click(screen.getByRole("button", { name: "Remove filter Unknown watch" }));
      expect(await screen.findByText("v1.0.0 published")).toBeInTheDocument();
      expect(router.state.location.search).toBe("?state=all");
    });
  });
});

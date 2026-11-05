import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { ME, mockApi } from "@/test/api";
import { renderApp } from "@/test/render";
import { lastCall, pending, problem } from "@/test/watches";
import type { AdminUser, AdminWatch } from "@/types/api";

const ADMIN = { ...ME, is_admin: true };

function account(over: Partial<AdminUser> = {}): AdminUser {
  return {
    id: "22222222-2222-2222-2222-222222222222",
    email: "bob@example.test",
    display_name: "Bob",
    created_at: "2026-01-01T00:00:00Z",
    last_login_at: "2026-11-01T10:00:00Z",
    status: "active",
    disabled_at: null,
    counts: { watches: 3, shared_watches: 1, channels: 2, rules: 4, scripts: 1, credentials: 1 },
    ...over,
  };
}

const ALICE = account({
  id: ME.id,
  email: ME.email,
  display_name: ME.display_name,
  counts: { watches: 0, shared_watches: 0, channels: 0, rules: 0, scripts: 0, credentials: 0 },
});

const DEE = account({
  id: "33333333-3333-3333-3333-333333333333",
  email: "dee@example.test",
  display_name: "Dee",
});

function watchOf(over: Partial<AdminWatch> = {}): AdminWatch {
  return {
    id: "44444444-4444-4444-4444-444444444444",
    name: "Go releases",
    kind: "github_release",
    credential_name: "gh",
    shares: 1,
    owner_rules: 2,
    ...over,
  };
}

const deferred = () => {
  let answer: (r: Response) => void = () => {};
  const route = () =>
    new Promise<Response>((resolve) => {
      answer = resolve;
    });
  return { route, answer: (r: Response) => answer(r) };
};

const dialog = (name: string) => screen.getByRole("dialog", { name });

const navLinks = () =>
  within(screen.getByRole("navigation", { name: "Main" }))
    .getAllByRole("link")
    .map((l) => l.textContent);

describe("AdminUsersPage", () => {
  // 006-AC5: the Administration entry shows for an admin only.
  it("shows the Administration entry to admins only", async () => {
    mockApi();
    const user = renderApp("/inbox");
    await screen.findByRole("heading", { level: 1, name: "Inbox" });
    expect(navLinks()).not.toContain("Administration");
    user.unmount();

    mockApi({ "/api/v1/me": ADMIN, "/api/v1/admin/users": { items: [] } });
    renderApp("/inbox", ADMIN);
    await screen.findByRole("heading", { level: 1, name: "Inbox" });
    expect(navLinks()).toContain("Administration");
    await userEvent.click(screen.getByRole("link", { name: "Administration" }));
    expect(await screen.findByRole("region", { name: "No accounts yet" })).toBeInTheDocument();
  });

  // 006-AC5: a non-admin opening the URL gets a no-access state and no admin call.
  it("tells a non-admin the page is for administrators", async () => {
    const fn = mockApi();
    renderApp("/admin/users");
    expect(await screen.findByRole("region", { name: "Administrators only" })).toBeInTheDocument();
    expect(lastCall(fn, "/api/v1/admin/users")).toBeUndefined();
  });

  // 006-AC6: one row per account with last sign-in, status and counts; the admin's row has no Disable.
  it("lists accounts with their status and counts", async () => {
    mockApi({
      "/api/v1/me": ADMIN,
      "/api/v1/admin/users": {
        items: [
          account({ id: "n", email: "new@example.test", display_name: "", last_login_at: null }),
          account(),
          account({ id: "d", email: "dee@example.test", display_name: "Dee", status: "disabled" }),
          account({ id: "p", email: "", display_name: "", status: "purged" }),
          ALICE,
        ],
      },
    });
    renderApp("/admin/users", ADMIN);
    const table = await screen.findByRole("table", { name: "Accounts" });
    await within(table).findByText("Bob");
    const rows = within(table).getAllByRole("row").slice(1);
    expect(rows).toHaveLength(5);
    expect(rows[0]).toHaveTextContent(/^new@example\.testNeverActive3 \(1 shared\)2411Disable$/);
    expect(rows[1]).toHaveTextContent("Bobbob@example.test");
    expect(within(rows[1] as HTMLElement).getByRole("time")).toHaveAttribute(
      "datetime",
      "2026-11-01T10:00:00Z",
    );
    expect(
      within(rows[2] as HTMLElement).getByRole("button", { name: "Enable Dee" }),
    ).toBeInTheDocument();
    expect(rows[3]).toHaveTextContent("Purged account");
    expect(within(rows[3] as HTMLElement).queryByRole("button")).toBeNull();
    expect(rows[4]).toHaveTextContent("(you)");
    expect(within(rows[4] as HTMLElement).queryByRole("button", { name: /Disable/ })).toBeNull();
  });

  // 006-AC7, 006-AC9: disable and enable go through a confirmation, then the list reloads.
  it("disables and enables after confirming", async () => {
    let bob = account();
    const fn = mockApi({
      "/api/v1/me": ADMIN,
      "/api/v1/admin/users": () => Response.json({ items: [bob, ALICE] }),
      [`/api/v1/admin/users/${bob.id}/disable`]: () => {
        bob = account({ status: "disabled", disabled_at: "2026-11-05T10:00:00Z" });
        return new Response(null, { status: 204 });
      },
      [`/api/v1/admin/users/${bob.id}/enable`]: () => {
        bob = account();
        return new Response(null, { status: 204 });
      },
    });
    renderApp("/admin/users", ADMIN);
    await userEvent.click(await screen.findByRole("button", { name: "Disable Bob" }));
    const dialog = screen.getByRole("dialog", { name: "Disable Bob?" });
    expect(dialog).toHaveTextContent("Signs the account out everywhere");
    await userEvent.click(within(dialog).getByRole("button", { name: "Disable" }));
    expect(await screen.findByText("Disabled Bob")).toBeInTheDocument();
    expect(await screen.findByRole("button", { name: "Enable Bob" })).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Enable Bob" }));
    const enable = screen.getByRole("dialog", { name: "Enable Bob?" });
    await userEvent.click(within(enable).getByRole("button", { name: "Enable" }));
    expect(await screen.findByRole("button", { name: "Disable Bob" })).toBeInTheDocument();
    expect(lastCall(fn, `/api/v1/admin/users/${bob.id}/enable`, "POST")).toBeDefined();
  });

  it("cancels without a call, and closes on a 409 so the reloaded row offers the right action", async () => {
    // Another admin disabled bob after this list was loaded.
    let bob = account();
    const fn = mockApi({
      "/api/v1/me": ADMIN,
      "/api/v1/admin/users": () => Response.json({ items: [bob] }),
      [`/api/v1/admin/users/${account().id}/disable`]: () => {
        bob = account({ status: "disabled" });
        return problem(409, "the account is already disabled");
      },
    });
    renderApp("/admin/users", ADMIN);
    await userEvent.click(await screen.findByRole("button", { name: "Disable Bob" }));
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(lastCall(fn, `/api/v1/admin/users/${account().id}/disable`, "POST")).toBeUndefined();

    await userEvent.click(screen.getByRole("button", { name: "Disable Bob" }));
    await userEvent.click(
      within(screen.getByRole("dialog")).getByRole("button", { name: "Disable" }),
    );
    expect(await screen.findByText(/already disabled/)).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(await screen.findByRole("button", { name: "Enable Bob" })).toBeInTheDocument();
  });

  it("keeps the dialog open with any other error", async () => {
    mockApi({
      "/api/v1/me": ADMIN,
      "/api/v1/admin/users": { items: [account()] },
      [`/api/v1/admin/users/${account().id}/disable`]: () => problem(500, "internal error"),
    });
    renderApp("/admin/users", ADMIN);
    await userEvent.click(await screen.findByRole("button", { name: "Disable Bob" }));
    await userEvent.click(
      within(screen.getByRole("dialog")).getByRole("button", { name: "Disable" }),
    );
    expect(await within(screen.getByRole("dialog")).findByRole("alert")).toHaveTextContent(
      "internal error",
    );
  });

  it("names the account and the action in the toast after a cancel while pending", async () => {
    let answer: (r: Response) => void = () => {};
    mockApi({
      "/api/v1/me": ADMIN,
      "/api/v1/admin/users": { items: [account()] },
      [`/api/v1/admin/users/${account().id}/disable`]: () =>
        new Promise<Response>((resolve) => {
          answer = resolve;
        }),
    });
    renderApp("/admin/users", ADMIN);
    await userEvent.click(await screen.findByRole("button", { name: "Disable Bob" }));
    await userEvent.click(
      within(screen.getByRole("dialog")).getByRole("button", { name: "Disable" }),
    );
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    answer(new Response(null, { status: 204 }));
    expect(await screen.findByText("Disabled Bob")).toBeInTheDocument();
  });

  it("has loading and error states", async () => {
    mockApi({ "/api/v1/me": ADMIN, "/api/v1/admin/users": pending });
    const first = renderApp("/admin/users", ADMIN);
    expect(await screen.findAllByTestId("user-skeleton")).toHaveLength(3);
    first.unmount();

    let fail = true;
    mockApi({
      "/api/v1/me": ADMIN,
      "/api/v1/admin/users": () =>
        fail ? new Response("", { status: 500 }) : Response.json({ items: [account()] }),
    });
    renderApp("/admin/users", ADMIN);
    expect(await screen.findByRole("alert")).toHaveTextContent("Accounts could not be loaded.");
    fail = false;
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("Bob")).toBeInTheDocument();
  });

  // 006-AC10: an account's watches open from its row; a transfer picks an active account, says what
  // moves and what is deleted, then reports the deleted rules.
  it("lists an account's watches and transfers one", async () => {
    const purged = account({ id: "p", email: "", display_name: "", status: "purged" });
    const off = account({ id: "d", display_name: "Off", status: "disabled" });
    const fn = mockApi({
      "/api/v1/me": ADMIN,
      "/api/v1/admin/users": { items: [account(), DEE, purged, off, ALICE] },
      [`/api/v1/admin/users/${account().id}/watches`]: { items: [watchOf()] },
      [`/api/v1/admin/watches/${watchOf().id}/transfer`]: () =>
        Response.json({
          owner_id: DEE.id,
          credential_id: "c",
          credential_moved: true,
          credential_name: "gh (transferred)",
          dropped_share: false,
          deleted_rules: [
            { id: "r1", name: "r1" },
            { id: "r2", name: "r2" },
          ],
        }),
    });
    renderApp("/admin/users", ADMIN);
    const toggle = await screen.findByRole("button", { name: "Watches of Bob" });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    await userEvent.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    const list = await screen.findByRole("list", { name: "Watches of Bob" });
    expect(list).toHaveTextContent(
      "Go releasesgithub_release · credential gh · shared with 1 user · 2 rules of the owner",
    );
    await userEvent.click(within(list).getByRole("button", { name: "Transfer Go releases" }));
    const d = dialog("Transfer Go releases?");
    expect(d).toHaveTextContent("Its credential gh moves with it");
    expect(d).toHaveTextContent("2 rules of Bob targeting it will be deleted");
    const confirm = within(d).getByRole("button", { name: "Transfer" });
    expect(confirm).toBeDisabled();
    const select = within(d).getByRole("combobox", { name: "New owner" });
    expect(
      within(select)
        .getAllByRole("option")
        .map((o) => o.textContent),
    ).toEqual([
      "Choose an active account",
      "Dee (dee@example.test)",
      "Alice Liddell (alice@example.test)",
    ]);
    await userEvent.selectOptions(select, DEE.id);
    await userEvent.click(confirm);
    expect(
      await screen.findByText("Transferred Go releases to Dee; deleted rules: r1, r2"),
    ).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(lastCall(fn, `/api/v1/admin/watches/${watchOf().id}/transfer`, "POST")?.body).toEqual({
      to_user_id: DEE.id,
    });

    await userEvent.click(toggle);
    expect(screen.queryByRole("list", { name: "Watches of Bob" })).toBeNull();
  });

  // 006-AC11: a refused transfer keeps the dialog open with the watches sharing the credential.
  it("shows a refused transfer in the dialog and reloads what it was built from", async () => {
    const fn = mockApi({
      "/api/v1/me": ADMIN,
      "/api/v1/admin/users": { items: [account(), DEE] },
      [`/api/v1/admin/users/${account().id}/watches`]: {
        items: [watchOf({ credential_name: null, owner_rules: 0, shares: 0 })],
      },
      [`/api/v1/admin/watches/${watchOf().id}/transfer`]: () =>
        problem(409, "the credential is also used by also, Zed"),
    });
    renderApp("/admin/users", ADMIN);
    await userEvent.click(await screen.findByRole("button", { name: "Watches of Bob" }));
    expect(await screen.findByText("github_release")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Transfer Go releases" }));
    const d = dialog("Transfer Go releases?");
    expect(d).not.toHaveTextContent("credential");
    expect(d).not.toHaveTextContent("will be deleted");
    await userEvent.selectOptions(within(d).getByRole("combobox"), DEE.id);
    const calls = (path: string) =>
      fn.mock.calls.filter(([input]) => new URL(String(input), "http://app.test").pathname === path)
        .length;
    const users = calls("/api/v1/admin/users");
    const watches = calls(`/api/v1/admin/users/${account().id}/watches`);
    await userEvent.click(within(d).getByRole("button", { name: "Transfer" }));
    expect(await within(d).findByRole("alert")).toHaveTextContent("also used by also, Zed");
    await waitFor(() => expect(calls("/api/v1/admin/users")).toBeGreaterThan(users));
    expect(calls(`/api/v1/admin/users/${account().id}/watches`)).toBeGreaterThan(watches);
  });

  it("keeps Purge disabled while a reopened dialog reloads its preview", async () => {
    const counts = {
      watches: 0,
      channels: 0,
      rules: 0,
      other_users_rules: 0,
      scripts: 0,
      credential_count: 0,
      shares_received: 0,
      inbox_items: 0,
      sessions: 0,
    };
    const second = deferred();
    let route: () => Response | Promise<Response> = () =>
      Response.json({ counts, shared_watches: [], other_rules: [] });
    mockApi({
      "/api/v1/me": ADMIN,
      "/api/v1/admin/users": { items: [account({ ...DEE, status: "disabled" })] },
      [`/api/v1/admin/users/${DEE.id}/purge-preview`]: () => route(),
    });
    renderApp("/admin/users", ADMIN);
    await userEvent.click(await screen.findByRole("button", { name: "Purge Dee" }));
    const purge = () => within(dialog("Purge Dee?")).getByRole("button", { name: "Purge" });
    await waitFor(() => expect(purge()).toBeEnabled());
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    route = second.route;
    await userEvent.click(screen.getByRole("button", { name: "Purge Dee" }));
    expect(await within(dialog("Purge Dee?")).findByTestId("purge-counts")).toBeInTheDocument();
    expect(purge()).toBeDisabled();
    second.answer(
      Response.json({ counts: { ...counts, rules: 3 }, shared_watches: [], other_rules: [] }),
    );
    await waitFor(() => expect(purge()).toBeEnabled());
    expect(within(dialog("Purge Dee?")).getByTestId("purge-counts")).toHaveTextContent("3 rules");
  });

  it("says when an account's watches cannot be loaded", async () => {
    mockApi({
      "/api/v1/me": ADMIN,
      "/api/v1/admin/users": { items: [account()] },
      [`/api/v1/admin/users/${account().id}/watches`]: () => problem(500, "internal error"),
    });
    renderApp("/admin/users", ADMIN);
    await userEvent.click(await screen.findByRole("button", { name: "Watches of Bob" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Watches could not be loaded: internal error",
    );
  });

  // 006-AC12, 006-AC13: only a disabled account offers Purge; its dialog lists what goes, what other
  // users lose, and the purged row is left without actions.
  it("purges a disabled account after showing what goes", async () => {
    let dee = account({ ...DEE, status: "disabled" });
    const fn = mockApi({
      "/api/v1/me": ADMIN,
      "/api/v1/admin/users": () => Response.json({ items: [account(), dee] }),
      [`/api/v1/admin/users/${DEE.id}/purge-preview`]: {
        counts: {
          watches: 1,
          channels: 2,
          rules: 1,
          other_users_rules: 1,
          scripts: 0,
          credential_count: 1,
          shares_received: 1,
          inbox_items: 4,
          sessions: 0,
        },
        shared_watches: [{ id: "w1", name: "w", shared_with: ["Bob", "Carl"] }],
        other_rules: [{ id: "r1", name: "r", owner: "Bob" }],
      },
      [`/api/v1/admin/users/${DEE.id}/purge`]: () => {
        dee = account({ id: DEE.id, email: "", display_name: "", status: "purged" });
        return new Response(null, { status: 204 });
      },
    });
    renderApp("/admin/users", ADMIN);
    expect(await screen.findByRole("button", { name: "Enable Dee" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Purge Bob" })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Purge Dee" }));
    const d = dialog("Purge Dee?");
    expect(await within(d).findByTestId("purge-counts")).toHaveTextContent(
      "1 watch, 2 channels, 1 rule, 0 scripts, 1 credential, 1 share received, 4 inbox items.",
    );
    expect(d).toHaveTextContent("Other users lose these:");
    expect(
      within(within(d).getByRole("list", { name: "Shared watches deleted" }))
        .getAllByRole("listitem")
        .map((l) => l.textContent),
    ).toEqual(["Watch w, shared with Bob, Carl", "Rule r of Bob"]);
    await userEvent.click(within(d).getByRole("button", { name: "Purge" }));
    expect(await screen.findByText("Purged Dee")).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(lastCall(fn, `/api/v1/admin/users/${DEE.id}/purge`, "POST")).toBeDefined();
    const row = (await screen.findByText("Purged account")).closest("tr") as HTMLElement;
    expect(row).toHaveTextContent("Purged");
    expect(within(row).queryByRole("button")).toBeNull();
  });

  it("purge dialog: loading, a preview without shares, a failed preview and a failed purge", async () => {
    const dee = account({ ...DEE, status: "disabled" });
    const empty = {
      counts: {
        watches: 0,
        channels: 0,
        rules: 0,
        other_users_rules: 0,
        scripts: 1,
        credential_count: 0,
        shares_received: 0,
        inbox_items: 0,
        sessions: 0,
      },
      shared_watches: [],
      other_rules: [],
    };
    const first = deferred();
    let preview: () => Response | Promise<Response> = first.route;
    mockApi({
      "/api/v1/me": ADMIN,
      "/api/v1/admin/users": { items: [dee] },
      [`/api/v1/admin/users/${DEE.id}/purge-preview`]: () => preview(),
      [`/api/v1/admin/users/${DEE.id}/purge`]: () => problem(500, "internal error"),
    });
    renderApp("/admin/users", ADMIN);
    await userEvent.click(await screen.findByRole("button", { name: "Purge Dee" }));
    expect(dialog("Purge Dee?")).toHaveTextContent("Loading what will be deleted");
    expect(within(dialog("Purge Dee?")).getByRole("button", { name: "Purge" })).toBeDisabled();
    first.answer(problem(500, "preview failed"));
    expect(await within(dialog("Purge Dee?")).findByRole("alert")).toHaveTextContent(
      "preview failed",
    );
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));

    preview = () => Response.json(empty);
    await userEvent.click(screen.getByRole("button", { name: "Purge Dee" }));
    const d = dialog("Purge Dee?");
    expect(await within(d).findByTestId("purge-counts")).toHaveTextContent("1 script");
    expect(d).not.toHaveTextContent("Other users lose these");
    await userEvent.click(within(d).getByRole("button", { name: "Purge" }));
    expect(await within(d).findByRole("alert")).toHaveTextContent("internal error");
  });

  it("closes the purge dialog on a 409 so the reloaded row offers the right action", async () => {
    let dee = account({ ...DEE, status: "disabled" });
    mockApi({
      "/api/v1/me": ADMIN,
      "/api/v1/admin/users": () => Response.json({ items: [dee] }),
      [`/api/v1/admin/users/${DEE.id}/purge-preview`]: {
        counts: {
          watches: 0,
          channels: 0,
          rules: 0,
          other_users_rules: 0,
          scripts: 0,
          credential_count: 0,
          shares_received: 0,
          inbox_items: 0,
          sessions: 0,
        },
        shared_watches: null,
        other_rules: null,
      },
      [`/api/v1/admin/users/${DEE.id}/purge`]: () => {
        dee = account({ ...DEE });
        return problem(409, "disable the account before purging it");
      },
    });
    renderApp("/admin/users", ADMIN);
    await userEvent.click(await screen.findByRole("button", { name: "Purge Dee" }));
    const d = dialog("Purge Dee?");
    await within(d).findByTestId("purge-counts");
    await userEvent.click(within(d).getByRole("button", { name: "Purge" }));
    expect(await screen.findByText(/before purging/)).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(await screen.findByRole("button", { name: "Disable Dee" })).toBeInTheDocument();
  });

  // The dialogs stay mounted: an answer arriving after Cancel must not fill or close the dialog
  // opened since for another account; it goes to a toast.
  describe("a mutation settling after Cancel", () => {
    const startThenSwitch = async () => {
      const bob = deferred();
      mockApi({
        "/api/v1/me": ADMIN,
        "/api/v1/admin/users": { items: [account(), DEE] },
        [`/api/v1/admin/users/${account().id}/disable`]: bob.route,
      });
      renderApp("/admin/users", ADMIN);
      await userEvent.click(await screen.findByRole("button", { name: "Disable Bob" }));
      await userEvent.click(
        within(dialog("Disable Bob?")).getByRole("button", { name: "Disable" }),
      );
      await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
      await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
      await userEvent.click(screen.getByRole("button", { name: "Disable Dee" }));
      expect(within(dialog("Disable Dee?")).getByRole("button", { name: "Disable" })).toBeEnabled();
      return bob;
    };

    it("toasts a late error instead of showing it in another account's dialog", async () => {
      const bob = await startThenSwitch();
      bob.answer(problem(500, "internal error"));
      expect(await screen.findByText("Bob: internal error")).toBeInTheDocument();
      const d = dialog("Disable Dee?");
      expect(within(d).queryByRole("alert")).toBeNull();
    });

    it("does not close another account's dialog on a late success", async () => {
      const bob = await startThenSwitch();
      bob.answer(new Response(null, { status: 204 }));
      expect(await screen.findByText("Disabled Bob")).toBeInTheDocument();
      expect(dialog("Disable Dee?")).toBeInTheDocument();
    });

    it("applies to purge and transfer too", async () => {
      const purge = deferred();
      const move = deferred();
      const off = (a: AdminUser) => ({ ...a, status: "disabled" as const });
      const preview = {
        counts: {
          watches: 0,
          channels: 0,
          rules: 0,
          other_users_rules: 0,
          scripts: 0,
          credential_count: 0,
          shares_received: 0,
          inbox_items: 0,
          sessions: 0,
        },
        shared_watches: [],
        other_rules: [],
      };
      const other = watchOf({ id: "55555555-5555-5555-5555-555555555555", name: "Other" });
      mockApi({
        "/api/v1/me": ADMIN,
        "/api/v1/admin/users": { items: [off(account()), off(DEE), ALICE] },
        [`/api/v1/admin/users/${account().id}/purge-preview`]: preview,
        [`/api/v1/admin/users/${DEE.id}/purge-preview`]: preview,
        [`/api/v1/admin/users/${account().id}/purge`]: purge.route,
        [`/api/v1/admin/users/${account().id}/watches`]: { items: [watchOf(), other] },
        [`/api/v1/admin/watches/${watchOf().id}/transfer`]: move.route,
      });
      renderApp("/admin/users", ADMIN);
      // Each answer once as an error (toast, nothing in the new dialog), once as a success (it stays open).
      const late = [
        {
          answer: problem(500, "internal error"),
          purge: "Bob: internal error",
          move: "Go releases: internal error",
        },
        {
          answer: new Response(null, { status: 204 }),
          purge: "Purged Bob",
          move: "Transferred Go releases to Alice Liddell",
        },
      ];
      for (const l of late) {
        await userEvent.click(await screen.findByRole("button", { name: "Purge Bob" }));
        await within(dialog("Purge Bob?")).findByTestId("purge-counts");
        await userEvent.click(within(dialog("Purge Bob?")).getByRole("button", { name: "Purge" }));
        await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
        await userEvent.click(screen.getByRole("button", { name: "Purge Dee" }));
        await within(dialog("Purge Dee?")).findByTestId("purge-counts");
        purge.answer(l.answer.clone());
        expect(await screen.findByText(l.purge)).toBeInTheDocument();
        expect(within(dialog("Purge Dee?")).queryByRole("alert")).toBeNull();
        await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
      }

      await userEvent.click(screen.getByRole("button", { name: "Watches of Bob" }));
      for (const l of late) {
        await userEvent.click(await screen.findByRole("button", { name: "Transfer Go releases" }));
        const d = dialog("Transfer Go releases?");
        await userEvent.selectOptions(within(d).getByRole("combobox"), ALICE.id);
        await userEvent.click(within(d).getByRole("button", { name: "Transfer" }));
        await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
        await userEvent.click(screen.getByRole("button", { name: "Transfer Other" }));
        move.answer(
          l.answer.status === 204
            ? Response.json({
                owner_id: ALICE.id,
                credential_id: null,
                credential_moved: false,
                credential_name: null,
                dropped_share: false,
                deleted_rules: [],
              })
            : l.answer.clone(),
        );
        expect(await screen.findByText(l.move)).toBeInTheDocument();
        expect(within(dialog("Transfer Other?")).queryByRole("alert")).toBeNull();
        await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
      }
    });
  });
});

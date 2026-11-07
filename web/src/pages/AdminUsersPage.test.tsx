import { focusManager } from "@tanstack/react-query";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { ME, mockApi } from "@/test/api";
import { renderApp } from "@/test/render";
import { FakeEventSource } from "@/test/routing";
import { lastCall, pending, problem } from "@/test/watches";
import type { AdminUser, AdminWatch, Me } from "@/types/api";

const ADMIN: Me = { ...ME, is_admin: true };
const ALICE_ADMIN = { id: ME.id, email: ME.email, display_name: ME.display_name };

function account(over: Partial<AdminUser> = {}): AdminUser {
  return {
    id: "22222222-2222-2222-2222-222222222222",
    email: "bob@example.test",
    display_name: "Bob",
    created_at: "2026-01-01T00:00:00Z",
    last_login_at: "2026-11-01T10:00:00Z",
    status: "active",
    disabled_at: null,
    counts: { watches: 3, channels: 2, rules: 4, scripts: 1, credentials: 1 },
    ...over,
  };
}

const ALICE = account({
  id: ME.id,
  email: ME.email,
  display_name: ME.display_name,
  counts: { watches: 0, channels: 0, rules: 0, scripts: 0, credentials: 0 },
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
    kind: "github_repo",
    credential_name: "gh",
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
          account({
            id: "n",
            email: "new@example.test",
            display_name: "",
            last_login_at: null,
            // Distinct counts, so a column swap shows.
            counts: { watches: 3, channels: 2, rules: 4, scripts: 1, credentials: 5 },
          }),
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
    expect(rows[0]).toHaveTextContent(/^new@example\.testNeverActive32415Act asDisable$/);
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

  // 006-AC15, 006-AC17: "Act as" on another live account (a disabled one too) opens its inbox under a
  // banner, without Administration; Stop brings the admin's own session back with everything refetched.
  it("acts as a user and stops", async () => {
    FakeEventSource.instances = [];
    vi.stubGlobal("EventSource", FakeEventSource);
    let me: Me = ADMIN;
    const bob = account();
    const fn = mockApi({
      "/api/v1/me": () => Response.json(me),
      "/api/v1/admin/users": {
        items: [
          bob,
          { ...DEE, status: "disabled" },
          account({ id: "p", email: "", display_name: "", status: "purged" }),
          ALICE,
        ],
      },
      [`/api/v1/admin/users/${bob.id}/act-as`]: () => {
        me = {
          ...ADMIN,
          id: bob.id,
          email: bob.email,
          display_name: "Bob",
          acting_as: ALICE_ADMIN,
        };
        return new Response(null, { status: 204 });
      },
      "/api/v1/admin/act-as": () => {
        me = ADMIN;
        return new Response(null, { status: 204 });
      },
    });
    const { router } = renderApp("/admin/users", ADMIN);
    const table = await screen.findByRole("table", { name: "Accounts" });
    await within(table).findByText("Bob");
    const rows = within(table).getAllByRole("row").slice(1);
    expect(
      within(rows[1] as HTMLElement).getByRole("button", { name: "Act as Dee" }),
    ).toBeEnabled();
    expect(within(rows[2] as HTMLElement).queryByRole("button", { name: /Act as/ })).toBeNull();
    expect(within(rows[3] as HTMLElement).queryByRole("button", { name: /Act as/ })).toBeNull();

    await userEvent.click(within(table).getByRole("button", { name: "Act as Bob" }));
    const banner = await screen.findByRole("region", { name: "Acting as another user" });
    expect(banner).toHaveTextContent("Acting as Bob");
    // The stream serves the user it opened for: it is reopened for bob.
    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(2));
    expect(FakeEventSource.instances[0]?.readyState).toBe(FakeEventSource.CLOSED);
    expect(lastCall(fn, `/api/v1/admin/users/${bob.id}/act-as`, "POST")).toBeDefined();
    expect(await screen.findByRole("heading", { level: 1, name: "Inbox" })).toBeInTheDocument();
    await waitFor(() => expect(navLinks()).not.toContain("Administration"));

    // Stop lands on the admin's inbox, not on a page of the user's.
    await act(() => router.navigate("/rules"));
    await screen.findByRole("heading", { level: 1, name: "Rules" });
    await userEvent.click(within(banner).getByRole("button", { name: "Stop" }));
    await waitFor(() =>
      expect(screen.queryByRole("region", { name: "Acting as another user" })).toBeNull(),
    );
    expect(router.state.location.pathname).toBe("/inbox");
    expect(lastCall(fn, "/api/v1/admin/act-as", "DELETE")).toBeDefined();
    expect(navLinks()).toContain("Administration");
    expect(FakeEventSource.instances).toHaveLength(3);
  });

  // 006-AC15: opened by URL while acting, the page says why and calls no admin API (it would answer 409).
  it("stays closed while acting", async () => {
    const acting: Me = { ...ADMIN, id: "b", display_name: "Bob", acting_as: ALICE_ADMIN };
    const fn = mockApi({ "/api/v1/me": acting });
    renderApp("/admin/users", acting);
    expect(await screen.findByRole("region", { name: "Stop acting first" })).toBeInTheDocument();
    expect(lastCall(fn, "/api/v1/admin/users")).toBeUndefined();
  });

  // Another tab started acting on this session: focusing this one refetches /me and shows the banner.
  it("learns of a switch made in another tab on focus", async () => {
    mockApi({ "/api/v1/me": ADMIN });
    renderApp("/inbox", ADMIN);
    await screen.findByRole("heading", { level: 1, name: "Inbox" });
    expect(screen.queryByRole("region", { name: "Acting as another user" })).toBeNull();
    mockApi({ "/api/v1/me": { ...ADMIN, id: "b", display_name: "Bob", acting_as: ALICE_ADMIN } });
    act(() => {
      focusManager.setFocused(false);
      focusManager.setFocused(true);
    });
    focusManager.setFocused(undefined);
    expect(await screen.findByRole("region", { name: "Acting as another user" })).toHaveTextContent(
      "Acting as Bob",
    );
  });

  it("reports a refused act-as and stays on the page", async () => {
    const bob = account();
    mockApi({
      "/api/v1/me": ADMIN,
      "/api/v1/admin/users": { items: [bob, ALICE] },
      [`/api/v1/admin/users/${bob.id}/act-as`]: () => problem(409, "the account is purged"),
    });
    renderApp("/admin/users", ADMIN);
    await userEvent.click(await screen.findByRole("button", { name: "Act as Bob" }));
    expect(await screen.findByText("Bob: the account is purged")).toBeInTheDocument();
    expect(screen.getByRole("heading", { level: 1, name: "Administration" })).toBeInTheDocument();
  });

  // A 409 means the session is already back to the admin (another tab, a purge): same landing as a stop.
  it("toasts a refused stop, refetches and leaves the user's page", async () => {
    const acting: Me = {
      ...ADMIN,
      id: "b",
      email: "b@x.test",
      display_name: "",
      acting_as: ALICE_ADMIN,
    };
    const fn = mockApi({
      "/api/v1/me": acting,
      "/api/v1/admin/act-as": () => problem(409, "this session is not acting as anyone"),
    });
    const { router } = renderApp("/rules", acting);
    const banner = await screen.findByRole("region", { name: "Acting as another user" });
    expect(banner).toHaveTextContent("Acting as b@x.test");
    const before = fn.mock.calls.length;
    await userEvent.click(within(banner).getByRole("button", { name: "Stop" }));
    expect(await screen.findByText("this session is not acting as anyone")).toBeInTheDocument();
    await waitFor(() => expect(fn.mock.calls.length).toBeGreaterThan(before + 1));
    expect(router.state.location.pathname).toBe("/inbox");
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
    // 009-AC12: the intro says disabling stops the account's watches too.
    expect(screen.getByText(/^Every account/)).toHaveTextContent(
      "Disabling one signs it out at once and stops its rules and watches; its data stays.",
    );
    const dialog = screen.getByRole("dialog", { name: "Disable Bob?" });
    // 009-AC12: a disabled account's watches do not collect.
    expect(dialog).toHaveTextContent(
      "Signs the account out everywhere and refuses its sign-in. Its rules route nothing and queued deliveries are not sent. Its watches stop collecting until it is enabled again. Its data stays.",
    );
    // 009-AC10: the disable dialog names no share.
    expect(dialog).not.toHaveTextContent(/shar/i);
    await userEvent.click(within(dialog).getByRole("button", { name: "Disable" }));
    expect(await screen.findByText("Disabled Bob")).toBeInTheDocument();
    expect(await screen.findByRole("button", { name: "Enable Bob" })).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Enable Bob" }));
    const enable = screen.getByRole("dialog", { name: "Enable Bob?" });
    // 009-AC12: re-enabled watches run again and report a diff against their last stored state, no replay.
    expect(enable).toHaveTextContent(
      "The account can sign in again, its rules route new changes and its watches run again.",
    );
    expect(enable).toHaveTextContent(
      "Each watch's next run reports what changed since its last run, as far as the forge still lists it.",
    );
    expect(enable).toHaveTextContent(
      "Inbox items skipped while disabled are not replayed; the owner can retry dead deliveries from the Channels page.",
    );
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

  // 006-AC10: an account's watches open from its row; a transfer picks an active account and says what
  // moves, never that rules are deleted.
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
        }),
    });
    renderApp("/admin/users", ADMIN);
    const toggle = await screen.findByRole("button", { name: "Watches of Bob" });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    await userEvent.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    const list = await screen.findByRole("list", { name: "Watches of Bob" });
    expect(list).toHaveTextContent("Go releasesgithub_repo · credential gh");
    await userEvent.click(within(list).getByRole("button", { name: "Transfer Go releases" }));
    const d = dialog("Transfer Go releases?");
    expect(d).toHaveTextContent(
      "Its credential gh moves with it, unless another watch of Bob uses it.",
    );
    expect(d).not.toHaveTextContent("rule");
    // 009-AC10: the transfer dialog names no share.
    expect(d).not.toHaveTextContent(/shar/i);
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
    expect(await screen.findByText("Transferred Go releases to Dee")).toBeInTheDocument();
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
        items: [watchOf({ credential_name: null })],
      },
      [`/api/v1/admin/watches/${watchOf().id}/transfer`]: () =>
        problem(409, "the credential is also used by also, Zed"),
    });
    renderApp("/admin/users", ADMIN);
    await userEvent.click(await screen.findByRole("button", { name: "Watches of Bob" }));
    expect(await screen.findByText("github_repo")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Transfer Go releases" }));
    const d = dialog("Transfer Go releases?");
    expect(d).not.toHaveTextContent("credential");
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
      api_key_count: 0,
      inbox_items: 0,
      sessions: 0,
    };
    const second = deferred();
    let route: () => Response | Promise<Response> = () =>
      Response.json({ counts, other_rules: [] });
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
    second.answer(Response.json({ counts: { ...counts, rules: 3 }, other_rules: [] }));
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
          api_key_count: 0,
          inbox_items: 4,
          sessions: 0,
        },
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
      "1 watch, 2 channels, 1 rule, 0 scripts, 1 credential, 0 API keys, 4 inbox items.",
    );
    expect(d).toHaveTextContent("Other users lose these:");
    expect(
      within(within(d).getByRole("list", { name: "Rules of other users deleted" }))
        .getAllByRole("listitem")
        .map((l) => l.textContent),
    ).toEqual(["Rule r of Bob"]);
    expect(d).toHaveTextContent("Transfer a watch first to keep it for them.");
    // 009-AC10: the purge dialog names no share.
    expect(d).not.toHaveTextContent(/shar/i);
    await userEvent.click(within(d).getByRole("button", { name: "Purge" }));
    expect(await screen.findByText("Purged Dee")).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(lastCall(fn, `/api/v1/admin/users/${DEE.id}/purge`, "POST")).toBeDefined();
    const row = (await screen.findByText("Purged account")).closest("tr") as HTMLElement;
    expect(row).toHaveTextContent("Purged");
    expect(within(row).queryByRole("button")).toBeNull();
  });

  it("purge dialog: loading, a preview without other users' rules, a failed preview and a failed purge", async () => {
    const dee = account({ ...DEE, status: "disabled" });
    const empty = {
      counts: {
        watches: 0,
        channels: 0,
        rules: 0,
        other_users_rules: 0,
        scripts: 1,
        credential_count: 0,
        api_key_count: 0,
        inbox_items: 0,
        sessions: 0,
      },
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
          api_key_count: 0,
          inbox_items: 0,
          sessions: 0,
        },
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
          api_key_count: 0,
          inbox_items: 0,
          sessions: 0,
        },
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

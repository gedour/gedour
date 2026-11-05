import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { ME, mockApi } from "@/test/api";
import { renderApp } from "@/test/render";
import { lastCall, pending, problem } from "@/test/watches";
import type { AdminUser } from "@/types/api";

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
});

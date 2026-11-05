import type { Browser, BrowserContext, Page } from "@playwright/test";

import {
  ALICE,
  api,
  BOB,
  DAVE,
  expect,
  gate,
  SEED,
  signInFromGate,
  test,
  UPSTREAM,
  watchCsp,
} from "./fixtures";

const BOB_EMAIL = "bob@example.test";

// One at a time, in file order: the transfer gives bob a watch, which a disabled bob would refuse.
test.describe.configure({ mode: "default" });

/** A new browser context with empty storage and its own CSP watch, added to contexts for closing. */
async function open(browser: Browser, baseURL: string | undefined, contexts: BrowserContext[]) {
  const context = await browser.newContext({ storageState: { cookies: [], origins: [] } });
  contexts.push(context);
  const page = await context.newPage();
  return { page, violations: await watchCsp(page, baseURL) };
}

/** Bob's status as the admin API reports it, re-enabled when asked: no later run starts with bob disabled. */
async function bobStatus(page: Page, enable = false): Promise<string> {
  const call = await api(page);
  const users = (await (await call("GET", "/api/v1/admin/users")).json()).items as {
    id: string;
    email: string;
    status: string;
  }[];
  const bob = users.find((u) => u.email === BOB_EMAIL);
  if (!bob) throw new Error("bob is not listed");
  if (enable && bob.status === "disabled") {
    expect((await call("POST", `/api/v1/admin/users/${bob.id}/enable`)).status()).toBe(204);
    return "active";
  }
  return bob.status;
}

// 006-AC5, 006-AC7, 006-AC9: alice (admin) disables bob from the Administration page; bob's live
// session drops to the gate and his next sign-in is refused; re-enabled, bob signs in again.
test("an admin disables then re-enables an account", async ({ page, browser, baseURL }) => {
  const violations: string[][] = [];
  const contexts: BrowserContext[] = [];
  try {
    // A sign-in of its own, not the stored bob session: a retry would find that one revoked.
    const live = await open(browser, baseURL, contexts);
    violations.push(live.violations);
    await live.page.goto("/inbox");
    await signInFromGate(live.page, BOB);
    await expect(live.page.getByRole("button", { name: `Account: ${BOB.name}` })).toBeVisible();

    await page.goto("/inbox");
    await page.getByRole("link", { name: "Administration" }).click();
    const table = page.getByRole("table", { name: "Accounts" });
    const row = table.getByRole("row").filter({ hasText: BOB_EMAIL });
    await expect(row).toContainText("Active");
    await row.getByRole("button", { name: `Disable ${BOB.name}` }).click();
    const confirm = page.getByRole("dialog", { name: `Disable ${BOB.name}?` });
    await confirm.getByRole("button", { name: "Disable" }).click();
    await expect(page.getByText(`Disabled ${BOB.name}`)).toBeVisible();
    await expect(row).toContainText("Disabled");

    // 006-AC7: the session bob had open is revoked at once.
    await live.page.reload();
    await expect(gate(live.page)).toBeVisible();
    expect((await live.page.request.get("/api/v1/me")).status()).toBe(401);

    // 006-AC7: a fresh sign-in is refused with the forbidden reason.
    const refused = await open(browser, baseURL, contexts);
    violations.push(refused.violations);
    await refused.page.goto("/inbox");
    await signInFromGate(refused.page, BOB);
    await expect(refused.page).toHaveURL(/\/inbox\?auth=forbidden$/);
    await expect(gate(refused.page, "No access to gedour")).toBeVisible();

    // 006-AC9: re-enabled, bob signs in again.
    await row.getByRole("button", { name: `Enable ${BOB.name}` }).click();
    await page
      .getByRole("dialog", { name: `Enable ${BOB.name}?` })
      .getByRole("button", { name: "Enable" })
      .click();
    await expect(row).toContainText("Active");
    const back = await open(browser, baseURL, contexts);
    violations.push(back.violations);
    await back.page.goto("/inbox");
    await signInFromGate(back.page, BOB);
    await expect(back.page.getByRole("button", { name: `Account: ${BOB.name}` })).toBeVisible();
  } finally {
    await Promise.all(contexts.map((c) => c.close()));
    expect(await bobStatus(page, true)).toBe("active");
  }
  expect(violations.flat()).toEqual([]);
});

// 006-AC10: alice (admin) gives one of her watches, with its credential, to bob from the
// Administration page, and back; alice cannot read it meanwhile.
test("an admin transfers a watch and back", async ({ page }) => {
  const call = await api(page);
  const me = (await (await call("GET", "/api/v1/me")).json()) as { id: string };
  const name = `transfer e2e ${Date.now()}`;
  const cred = await call("POST", "/api/v1/credentials", {
    name,
    kind: "github_token",
    secret: "e2e-transfer-secret-0123456789",
  });
  expect(cred.status()).toBe(201);
  const credId = ((await cred.json()) as { id: string }).id;
  const created = await call("POST", "/api/v1/watches", {
    name,
    kind: "github_release",
    schedule: "1h",
    credential_id: credId,
    config: { repo: "cli/cli", api_url: UPSTREAM },
  });
  expect(created.status()).toBe(201);
  const id = ((await created.json()) as { id: string }).id;
  const users = (await (await call("GET", "/api/v1/admin/users")).json()).items as {
    id: string;
    email: string;
  }[];
  const bobId = users.find((u) => u.email === BOB_EMAIL)?.id as string;
  const ownedBy = async (user: string) =>
    (
      (await (await call("GET", `/api/v1/admin/users/${user}/watches`)).json()).items as {
        id: string;
        credential_name: string | null;
      }[]
    ).find((w) => w.id === id);
  type Account = { name: string; email: string };
  const alice = { name: ALICE.name, email: "alice@example.test" };
  const bob = { name: BOB.name, email: BOB_EMAIL };
  const give = async (from: Account, to: Account) => {
    const row = page
      .getByRole("table", { name: "Accounts" })
      .getByRole("row")
      .filter({ hasText: from.email });
    await row.getByRole("button", { name: `Watches of ${from.name}` }).click();
    await page.getByRole("button", { name: `Transfer ${name}` }).click();
    const dialog = page.getByRole("dialog", { name: `Transfer ${name}?` });
    await expect(dialog).toContainText(`Its credential ${name} moves with it`);
    await dialog
      .getByRole("combobox", { name: "New owner" })
      .selectOption({ label: `${to.name} (${to.email})` });
    await dialog.getByRole("button", { name: "Transfer" }).click();
    await expect(page.getByText(`Transferred ${name} to ${to.name}`)).toBeVisible();
    await expect(dialog).toBeHidden();
  };
  try {
    await page.goto("/admin/users");
    await give(alice, bob);
    expect((await ownedBy(bobId))?.credential_name).toBe(name);
    expect((await call("GET", `/api/v1/watches/${id}`)).status()).toBe(404);
    expect((await call("GET", `/api/v1/credentials/${credId}`)).status()).toBe(404);

    await give(bob, alice);
    expect((await ownedBy(me.id))?.credential_name).toBe(name);
    expect((await call("GET", `/api/v1/watches/${id}`)).status()).toBe(200);
  } finally {
    // Bob ends as he began: the watch comes back to alice, then goes with its credential.
    if (await ownedBy(bobId)) {
      expect(
        (
          await call("POST", `/api/v1/admin/watches/${id}/transfer`, { to_user_id: me.id })
        ).status(),
      ).toBe(200);
    }
    expect((await call("DELETE", `/api/v1/watches/${id}`)).status()).toBe(204);
    expect((await call("DELETE", `/api/v1/credentials/${credId}`)).status()).toBe(204);
  }
  expect(await ownedBy(bobId)).toBeUndefined();
});

// 006-AC12, 006-AC13, 006-AC14: dave, the realm's disposable user, signs in once; alice disables then
// purges him through the confirmation, his row shows purged without actions, and his sign-in is refused.
test("an admin purges a disabled account", async ({ page, browser, baseURL }) => {
  const violations: string[][] = [];
  const contexts: BrowserContext[] = [];
  const call = await api(page);
  const users = async () =>
    (await (await call("GET", "/api/v1/admin/users")).json()).items as {
      id: string;
      email: string;
      status: string;
    }[];
  // Where a previous attempt (a CI retry, a run that died) left dave. A purged row has a blank email; it
  // can only be dave's, the one account the suite purges.
  const daveState = async (): Promise<"new" | "active" | "disabled" | "purged"> => {
    const all = await users();
    const dave = all.find((u) => u.email === "dave@example.test");
    if (dave) return dave.status as "active" | "disabled";
    return all.some((u) => u.status === "purged") ? "purged" : "new";
  };
  const signIn = async () => {
    const ctx = await open(browser, baseURL, contexts);
    violations.push(ctx.violations);
    await ctx.page.goto("/inbox");
    await signInFromGate(ctx.page, DAVE);
    return ctx.page;
  };
  try {
    const state = await daveState();
    if (state !== "purged") {
      if (state === "new" || state === "active") {
        const first = await signIn();
        await expect(first.getByRole("button", { name: `Account: ${DAVE.name}` })).toBeVisible();
      }
      const id = (await users()).find((u) => u.email === "dave@example.test")?.id as string;
      await page.goto("/admin/users");
      const table = page.getByRole("table", { name: "Accounts" });
      const row = table.getByRole("row").filter({ hasText: "dave@example.test" });
      if (state !== "disabled") {
        await row.getByRole("button", { name: `Disable ${DAVE.name}` }).click();
        await page
          .getByRole("dialog", { name: `Disable ${DAVE.name}?` })
          .getByRole("button", { name: "Disable" })
          .click();
      }
      await expect(row).toContainText("Disabled");
      await row.getByRole("button", { name: `Purge ${DAVE.name}` }).click();
      const confirm = page.getByRole("dialog", { name: `Purge ${DAVE.name}?` });
      await expect(confirm).toContainText("This cannot be undone");
      await expect(confirm.getByTestId("purge-counts")).toBeVisible();
      await confirm.getByRole("button", { name: "Purge" }).click();
      await expect(page.getByText(`Purged ${DAVE.name}`)).toBeVisible();
      await expect(confirm).toBeHidden();
      expect((await users()).find((u) => u.id === id)?.status).toBe("purged");
      const purged = table.getByRole("row").filter({ hasText: "Purged account" });
      await expect(purged).toHaveCount(1);
      await expect(purged).toContainText("Purged");
      await expect(purged.getByRole("button")).toHaveCount(0);
    }

    const again = await signIn();
    await expect(again).toHaveURL(/\/inbox\?auth=forbidden$/);
    await expect(gate(again, "No access to gedour")).toBeVisible();
    expect(await daveState()).toBe("purged");
  } finally {
    await Promise.all(contexts.map((c) => c.close()));
  }
  expect(violations.flat()).toEqual([]);
});

// 006-AC15, 006-AC16, 006-AC17: alice (admin) acts as bob from the Administration page: bob's data under a
// banner, without Administration; a rule she creates is bob's; Stop brings her own session back; bob's
// audit view shows the entry as made by alice for bob.
test("an admin acts as a user", async ({ page, browser, baseURL }) => {
  const violations: string[][] = [];
  const contexts: BrowserContext[] = [];
  const call = await api(page);
  const users = (await (await call("GET", "/api/v1/admin/users")).json()).items as {
    id: string;
    email: string;
  }[];
  const bobId = users.find((u) => u.email === BOB_EMAIL)?.id as string;
  const name = `act-as e2e ${Date.now()}`;
  let ruleId = "";
  const banner = page.getByRole("region", { name: "Acting as another user" });
  try {
    await page.goto("/admin/users");
    const row = page
      .getByRole("table", { name: "Accounts" })
      .getByRole("row")
      .filter({ hasText: BOB_EMAIL });
    await row.getByRole("button", { name: `Act as ${BOB.name}` }).click();
    await expect(banner).toContainText(`Acting as ${BOB.name}`);
    await expect(page).toHaveURL(/\/inbox$/);
    await expect(page.getByRole("link", { name: "Administration" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: `Account: ${BOB.name}` })).toBeVisible();

    const created = await call("POST", "/api/v1/rules", { name });
    expect(created.status()).toBe(201);
    ruleId = ((await created.json()) as { id: string }).id;
    await page.getByRole("link", { name: "Rules", exact: true }).click();
    const rules = page.getByRole("list", { name: "Rules" });
    await expect(rules).toContainText(SEED.rule);
    await expect(rules).toContainText(name);

    await banner.getByRole("button", { name: "Stop" }).click();
    await expect(banner).toBeHidden();
    await expect(page.getByRole("link", { name: "Administration" })).toBeVisible();
    await expect(page.getByRole("button", { name: `Account: ${ALICE.name}` })).toBeVisible();
    await expect(page.getByText(SEED.rule)).toHaveCount(0);
    await expect(page.getByText(name)).toHaveCount(0);

    // A sign-in of its own: the disable test revoked bob's stored session.
    const ctx = await open(browser, baseURL, contexts);
    violations.push(ctx.violations);
    await ctx.page.goto("/audit");
    await signInFromGate(ctx.page, BOB);
    const entry = ctx.page
      .getByRole("table", { name: "Audit entries, newest first" })
      .getByRole("row")
      .filter({ hasText: "rule.created" })
      .filter({ hasText: `${ruleId.slice(0, 8)}…` });
    await expect(entry).toContainText(`${ALICE.name} for ${BOB.name}`);
  } finally {
    await Promise.all(contexts.map((c) => c.close()));
    // Bob loses the rule and alice leaves act-as, whatever step failed.
    if (ruleId) {
      const status = (await call("POST", `/api/v1/admin/users/${bobId}/act-as`)).status();
      expect([204, 409]).toContain(status);
      expect((await call("DELETE", `/api/v1/rules/${ruleId}`)).status()).toBe(204);
    }
    await call("DELETE", "/api/v1/admin/act-as");
    const me = (await (await call("GET", "/api/v1/me")).json()) as { acting_as: unknown };
    expect(me.acting_as).toBeNull();
  }
  expect(violations.flat()).toEqual([]);
});

import type { Browser, BrowserContext, Page } from "@playwright/test";

import { api, BOB, expect, gate, signInFromGate, test, watchCsp } from "./fixtures";

const BOB_EMAIL = "bob@example.test";

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

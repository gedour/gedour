import { CSRF_PROBLEM_TITLE } from "../src/api/client";
import { ALICE, BOB, expect, signInAtProvider, test } from "./fixtures";

test.describe("signed out", () => {
  test.use({ storageState: { cookies: [], origins: [] } });

  // 001-AC1: without a session the API is 401 and a UI route goes to the provider, then back.
  test("unauthenticated API is 401, UI goes through the provider login", async ({
    page,
    request,
  }) => {
    const res = await request.get("/api/v1/me");
    expect(res.status()).toBe(401);
    expect(res.headers()["content-type"]).toContain("application/problem+json");
    expect((await request.get("/api/v1/version")).status()).toBe(200);

    await page.goto("/audit?from=x");
    await signInAtProvider(page, ALICE);
    await expect(page).toHaveURL(/\/audit\?from=x$/);
    await expect(page.getByRole("button", { name: `Account: ${ALICE.name}` })).toBeVisible();
  });

  // 001-AC3 / 001-AC21: sign-out needs the CSRF token, ends both the gedour and the provider
  // session, and lands on a page that stays signed out.
  test("sign out ends the session at gedour and at the provider", async ({ page }) => {
    await page.goto("/inbox");
    await signInAtProvider(page, BOB);
    const account = page.getByRole("button", { name: `Account: ${BOB.name}` });
    await expect(account).toBeVisible();

    expect((await page.request.post("/auth/logout")).status()).toBe(403);
    expect((await page.request.get("/api/v1/me")).status()).toBe(200);

    await account.click();
    await expect(page.getByRole("menu")).toContainText("bob@example.test");
    await page.getByRole("menuitem", { name: "Sign out" }).click();
    await expect(page).toHaveURL(/\/auth\/signed-out$/);
    await expect(page.getByRole("heading", { name: "You are signed out" })).toBeVisible();
    expect((await page.request.get("/api/v1/me")).status()).toBe(401);

    await page.reload();
    await expect(page.getByRole("heading", { name: "You are signed out" })).toBeVisible();
    await expect(account).toBeHidden();

    // The provider session is gone too: signing in again asks for credentials.
    await page.getByRole("link", { name: "Sign in again" }).click();
    await signInAtProvider(page, BOB);
    await expect(account).toBeVisible();
    await page.goto("/audit");
    await page.getByLabel("Target type").selectOption("session");
    await expect(page.getByRole("table", { name: /Audit entries/ })).toContainText(
      "session.logout",
    );
  });
});

// 001-AC3: a state-changing call without the CSRF header is refused; with it, it reaches routing.
test("mutations without the CSRF token are 403", async ({ page }) => {
  await page.goto("/inbox");
  const me = await (await page.request.get("/api/v1/me")).json();
  const without = await page.request.post("/api/v1/me");
  expect(without.status()).toBe(403);
  expect(without.headers()["content-type"]).toContain("application/problem+json");
  // The client retries only on this title: pin it against the real server.
  expect((await without.json()).title).toBe(CSRF_PROBLEM_TITLE);
  expect(
    (await page.request.post("/api/v1/me", { headers: { "X-CSRF-Token": "forged" } })).status(),
  ).toBe(403);
  const withToken = await page.request.post("/api/v1/me", {
    headers: { "X-CSRF-Token": me.csrf_token },
  });
  expect(withToken.status()).toBe(405);
});

// 001-AC22: the audit page lists the login, newest first, and opens its details.
test("audit page lists the sign-in", async ({ page }) => {
  await page.goto("/audit");
  const table = page.getByRole("table", { name: "Audit entries, newest first" });
  const login = table.getByRole("row").filter({ hasText: "user.login" }).first();
  await expect(login).toContainText(ALICE.name);
  await login.getByRole("button", { name: /Show details of user.login/ }).click();
  await expect(page.getByRole("table", { name: "Changes recorded by user.login" })).toContainText(
    "session",
  );

  await page.getByLabel("Target type").selectOption("session");
  await page.getByLabel("Target type").selectOption("user");
  await expect(table.getByRole("row").filter({ hasText: "user.created" })).toHaveCount(1);
});

test("user menu shows the signed-in user without CSP violations", async ({ page }) => {
  await page.goto("/inbox");
  await page.getByRole("button", { name: `Account: ${ALICE.name}` }).click();
  const menu = page.getByRole("menu");
  await expect(menu).toContainText(ALICE.name);
  await expect(menu).toContainText("alice@example.test");
  await expect(menu.getByRole("menuitem", { name: "Sign out" })).toBeVisible();
});

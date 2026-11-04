import type { Page } from "@playwright/test";

import { ALICE, expect, gate, signInAtProvider, test } from "./fixtures";

const sidebar = (page: Page) => page.getByRole("complementary", { name: "Sidebar" });

// alice's stored session: clearing cookies here only drops this context's copy, never the server session.
test.describe("session lost mid-session", () => {
  // 002-AC3: the session cookie goes away; the next API call answers 401 and the gate replaces the app.
  test("the next API call after the session is gone shows the gate", async ({ page, context }) => {
    await page.goto("/inbox");
    await expect(page.getByRole("button", { name: `Account: ${ALICE.name}` })).toBeVisible();
    await context.clearCookies();
    const unauthorized = page.waitForResponse(
      (r) => new URL(r.url()).pathname === "/api/v1/watches" && r.status() === 401,
    );
    await sidebar(page).getByRole("link", { name: "Watches" }).click();
    await unauthorized;
    await expect(gate(page)).toBeVisible();
    await expect(sidebar(page)).toHaveCount(0);
    await expect(page.getByRole("alert")).toHaveCount(0);
    await expect(page.getByRole("link", { name: "Sign in" })).toHaveAttribute(
      "href",
      "/auth/login?return_to=%2Fwatches",
    );
  });

  // 002-AC4: the stream's signed_out event shows the gate without user action.
  test("the stream's signed_out shows the gate", async ({ page }) => {
    await page.route("**/api/v1/inbox/stream", (route) =>
      route.fulfill({
        status: 200,
        contentType: "text/event-stream",
        body: 'event: inbox\ndata: {"unread":0}\n\nevent: signed_out\ndata: {}\n\n',
      }),
    );
    await page.goto("/rules");
    await expect(gate(page)).toBeVisible();
    await expect(sidebar(page)).toHaveCount(0);
  });

  // 002-AC4: the browser's own retries keep the stream CONNECTING; its errors recheck the session at
  // most every 30 s: still valid, the app stays; gone, the gate shows.
  test("a stream drop with the session gone shows the gate", async ({ page, context }) => {
    await page.clock.install();
    // Each answer ends at once: the browser reconnects after `retry`, so the stream keeps erroring.
    await page.route("**/api/v1/inbox/stream", (route) =>
      route.fulfill({
        status: 200,
        contentType: "text/event-stream",
        body: 'retry: 300\nevent: inbox\ndata: {"unread":0}\n\n',
      }),
    );
    // Only the session check may answer 401: the inbox refetches each drop triggers stay 200.
    await page.route(/\/api\/v1\/inbox(\/count)?(\?|$)/, (route) =>
      route.fulfill({
        json: route.request().url().includes("/count") ? { unread: 0 } : { items: [] },
      }),
    );
    const me: number[] = [];
    page.on("response", (r) => {
      if (new URL(r.url()).pathname === "/api/v1/me") me.push(r.status());
    });
    // Settings polls nothing, so the clock jump below fires no other call.
    await page.goto("/settings");
    await expect(page.getByRole("heading", { level: 1, name: "Settings" })).toBeVisible();
    // The boot check, then the first stream error's check, both still signed in.
    await expect.poll(() => me).toEqual([200, 200]);
    // The stream keeps erroring every 300 ms; none of it checks again within 30 s.
    await page.waitForTimeout(1_000);
    expect(me).toEqual([200, 200]);
    await expect(sidebar(page)).toBeVisible();

    await context.clearCookies();
    await page.clock.fastForward(30_000);
    await expect(gate(page)).toBeVisible();
    await expect(sidebar(page)).toHaveCount(0);
    await expect.poll(() => me).toEqual([200, 200, 401]);
  });
});

test.describe("sign-in failures", () => {
  test.use({ storageState: { cookies: [], origins: [] } });

  // 002-AC5: an unreachable provider brings the gate back with its reason (the server's answer is
  // simulated: the running server has already discovered Keycloak); the retry then signs in.
  test("provider unavailable shows the reason and a working retry", async ({ page }) => {
    await page.goto("/watches?q=cli");
    await expect(gate(page)).toBeVisible();
    await page.route("**/auth/login?**", (route) =>
      route.fulfill({ status: 303, headers: { Location: "/watches?auth=unavailable&q=cli" } }),
    );
    await page.getByRole("link", { name: "Sign in" }).click();
    await expect(gate(page, "Sign-in service unavailable")).toBeVisible();
    await expect(page.locator("body")).not.toContainText("identity provider unavailable");
    await page.unroute("**/auth/login?**");

    await page.getByRole("link", { name: "Try again" }).click();
    await signInAtProvider(page, ALICE);
    await expect(page).toHaveURL(/\/watches\?q=cli$/);
    await expect(page.getByRole("button", { name: `Account: ${ALICE.name}` })).toBeVisible();
  });

  // 002-AC6: real server: a callback without its login state lands on the gate, not a text page.
  test("a callback without login state shows 'Sign-in failed'", async ({ page }) => {
    const res = await page.goto("/auth/callback?code=x&state=y");
    expect(res?.headers()["content-type"]).toContain("text/html");
    await expect(page).toHaveURL(/\/\?auth=failed$/);
    await expect(gate(page, "Sign-in failed, try again")).toBeVisible();
    await expect(page.getByRole("link", { name: "Try again" })).toHaveAttribute(
      "href",
      "/auth/login?return_to=%2F",
    );
  });

  // 002-AC6: real server: a forged state after a real login start returns to the requested page.
  test("a state mismatch shows 'Sign-in failed' on the requested page", async ({ page }) => {
    await page.goto("/rules");
    await page.getByRole("link", { name: "Sign in" }).click();
    await expect(page).toHaveURL(/\/realms\/gedour-test\/protocol\/openid-connect\/auth/);
    await page.goto("/auth/callback?code=x&state=forged");
    await expect(page).toHaveURL(/\/rules\?auth=failed$/);
    await expect(gate(page, "Sign-in failed, try again")).toBeVisible();
    await expect(sidebar(page)).toHaveCount(0);
  });
});

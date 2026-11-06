import { AUTH_FILE, BOB_AUTH_FILE } from "./auth-file";
import { ALICE, BOB, expect, signInFromGate, test } from "./fixtures";

// Signs alice in once through Keycloak; the other specs reuse the stored session.
test("sign in as alice", async ({ page }) => {
  await page.goto("/inbox");
  await signInFromGate(page, ALICE);
  await expect(page.getByRole("button", { name: `Account: ${ALICE.name}` })).toBeVisible();
  // 006-AC4: the realm's admin role reaches /me through the ID token's roles claim.
  expect((await (await page.request.get("/api/v1/me")).json()).is_admin).toBe(true);
  await page.context().storageState({ path: AUTH_FILE });
});

// Bob signs in too: the seed, a11y, keyboard and API key specs run as him, and he must not reach alice's watches.
test("sign in as bob", async ({ page }) => {
  await page.goto("/inbox");
  await signInFromGate(page, BOB);
  await expect(page.getByRole("button", { name: `Account: ${BOB.name}` })).toBeVisible();
  // 006-AC4: the user role alone is not admin.
  expect((await (await page.request.get("/api/v1/me")).json()).is_admin).toBe(false);
  await page.context().storageState({ path: BOB_AUTH_FILE });
});

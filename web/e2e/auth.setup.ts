import { AUTH_FILE, BOB_AUTH_FILE } from "./auth-file";
import { ALICE, BOB, expect, signInFromGate, test } from "./fixtures";

// Signs alice in once through Keycloak; the other specs reuse the stored session.
test("sign in as alice", async ({ page }) => {
  await page.goto("/inbox");
  await signInFromGate(page, ALICE);
  await expect(page.getByRole("button", { name: `Account: ${ALICE.name}` })).toBeVisible();
  await page.context().storageState({ path: AUTH_FILE });
});

// Bob signs in too: sharing needs a second account that exists before alice shares with it.
test("sign in as bob", async ({ page }) => {
  await page.goto("/inbox");
  await signInFromGate(page, BOB);
  await expect(page.getByRole("button", { name: `Account: ${BOB.name}` })).toBeVisible();
  await page.context().storageState({ path: BOB_AUTH_FILE });
});

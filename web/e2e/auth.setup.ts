import { AUTH_FILE } from "./auth-file";
import { ALICE, expect, signInAtProvider, test } from "./fixtures";

// Signs alice in once through Keycloak; the other specs reuse the stored session.
test("sign in as alice", async ({ page }) => {
  await page.goto("/inbox");
  await signInAtProvider(page, ALICE);
  await expect(page.getByRole("button", { name: `Account: ${ALICE.name}` })).toBeVisible();
  await page.context().storageState({ path: AUTH_FILE });
});

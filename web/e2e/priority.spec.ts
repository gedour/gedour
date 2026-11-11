import { BOB_AUTH_FILE } from "./auth-file";
import { expect, SEED, test } from "./fixtures";

test.use({ storageState: BOB_AUTH_FILE });

// 005-AC2, 005-AC5: bob's seeded rule on `len(event.payload.cves) > 0` took the two releases with CVE ids:
// they carry the badge, the filter lists only them and keeps its URL state. 005-AC7: the fixtures' CSP guard covers both.
test("a high rule on a fixture release shows the badge and the filter", async ({ page }) => {
  await page.goto("/inbox?state=all");
  const row = (name: RegExp) =>
    page
      .getByRole("list", { name: /^Inbox, / })
      .getByRole("listitem")
      .filter({ hasText: name });
  await expect(row(/v2\.105\.0 published/).getByText("High priority")).toBeVisible();
  await expect(row(/v2\.103\.3 published/).getByText("High priority")).toBeVisible();
  await expect(row(/promoted to release/)).toBeVisible();
  await expect(row(/promoted to release/).getByText("High priority")).toHaveCount(0);

  await page.getByRole("button", { name: "High priority", exact: true }).click();
  await expect(page).toHaveURL(/\?state=all&priority=high$/);
  await expect(row(/promoted to release/)).toHaveCount(0);
  // Waits out a refetch: bob's inbox changes under parallel specs (read marks from push.spec.ts too).
  await expect(page.getByRole("list", { name: /^Inbox, / }).getByRole("listitem")).toHaveCount(2);
  const titles = await page
    .getByRole("list", { name: /^Inbox, / })
    .getByRole("listitem")
    .allInnerTexts();
  expect(titles).toHaveLength(2);
  expect(titles.every((t) => t.includes("High priority"))).toBe(true);

  await page.reload();
  await expect(page.getByRole("button", { name: "High priority", exact: true })).toHaveAttribute(
    "aria-pressed",
    "true",
  );
  await expect(row(/promoted to release/)).toHaveCount(0);

  await row(/v2\.105\.0 published/)
    .getByRole("button")
    .first()
    .click();
  await expect(page.getByRole("complementary").getByText("High priority")).toBeVisible();
});

// 005-AC1: the seeded high rule reads high in the list and its form.
test("a high rule shows in the rules list and its form", async ({ page }) => {
  await page.goto("/rules");
  const item = page
    .getByRole("list", { name: "Rules" })
    .getByRole("listitem")
    .filter({ hasText: SEED.highRule });
  await expect(item.getByText("High priority")).toBeVisible();
  await item.getByRole("link", { name: SEED.highRule }).click();
  await expect(page.getByLabel("Priority")).toHaveValue("high");
});

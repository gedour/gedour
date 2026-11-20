import type { Page } from "@playwright/test";

import { BOB_AUTH_FILE } from "./auth-file";
import { api, expect, SEED, test } from "./fixtures";

test.use({ storageState: BOB_AUTH_FILE, hasTouch: true });

const SHOTS = process.env.E2E_SHOTS_DIR;

// The list and the read calls are served here: a bulk read of bob's real inbox would race the
// keyboard spec's read toggles.
async function serveInbox(page: Page) {
  await page.goto("/inbox");
  const watches = (await (await (await api(page))("GET", "/api/v1/watches")).json()).items as {
    id: string;
    name: string;
    kind: string;
  }[];
  const seeded = watches.find((w) => w.name === SEED.watch);
  if (!seeded) throw new Error("seed.setup.ts did not run");
  // Long: the 375 px dialog must truncate it in the per-watch button, not scroll sideways.
  const other = {
    id: "00000000-0000-4000-8000-000000000020",
    name: "example-org/very-long-project-name releases",
    kind: "rss",
  };
  const at = new Date().toISOString();
  const item = (id: number, watch: { id: string }) => ({
    id,
    created_at: at,
    read_at: null as string | null,
    archived_at: null,
    priority: "normal",
    event: {
      id,
      type: "release.published",
      occurred_at: null,
      observed_at: at,
      payload: { tag: `v${id}.0.0` },
    },
    watch,
  });
  const items = [item(3, seeded), item(2, other), item(1, seeded)];
  const asked: URL[] = [];
  await page.route("**/api/v1/inbox?*", (route) => {
    const url = new URL(route.request().url());
    asked.push(url);
    const watch = url.searchParams.get("watch");
    const unread = url.searchParams.get("state") === "unread";
    return route.fulfill({
      json: {
        items: items.filter((i) => (!watch || i.watch.id === watch) && (!unread || !i.read_at)),
      },
    });
  });
  let readIds: number[] = [];
  await page.route("**/api/v1/inbox/read", async (route) => {
    readIds = (route.request().postDataJSON() as { ids: number[] }).ids;
    for (const i of items) if (readIds.includes(i.id)) i.read_at = at;
    await route.fulfill({ json: { updated: readIds.length, unread: 1 } });
  });
  const watchReads: string[] = [];
  await page.route("**/api/v1/inbox/watches/*/read", (route) => {
    watchReads.push(new URL(route.request().url()).pathname);
    // Nothing changes on the list: the count in the toast is all this mock answers.
    return route.fulfill({ json: { updated: 1, unread: 1 } });
  });
  await page.reload();
  return { seeded, other, asked, readIds: () => readIds, watchReads };
}

// 020-AC5, 020-AC6, 020-AC7, 020-AC8, 020-AC10: filter by a watch, reload, select all, mark read, remove
// the chip, then tap the link and the row at 375 px. 020-AC15: mark all from a watch read from the 375 px
// dialog.
test("filter the inbox by a watch and mark its items read", async ({ page }) => {
  const { seeded, other, asked, readIds, watchReads } = await serveInbox(page);

  const rows = page.getByRole("list", { name: /^Inbox, / }).getByRole("listitem");
  await expect(rows).toHaveCount(3);
  await rows
    .first()
    .getByRole("link", { name: `Show only ${SEED.watch}` })
    .click();
  await expect(page).toHaveURL(new RegExp(`/inbox\\?watch=${seeded.id}$`));
  await expect(rows).toHaveCount(2);
  expect(asked.at(-1)?.searchParams.get("watch")).toBe(seeded.id);
  await expect(page.getByRole("complementary", { name: /published/ })).toHaveCount(0);
  const chip = page.getByRole("button", { name: `Remove filter ${SEED.watch}` });
  await expect(chip).toBeVisible();
  if (SHOTS) {
    await page.screenshot({ path: `${SHOTS}/020-1280-filtered.png` });
    await page.setViewportSize({ width: 375, height: 812 });
  }
  // 020-AC7: a reload keeps the filter.
  await page.reload();
  await expect(chip).toBeVisible();
  await expect(rows).toHaveCount(2);
  if (SHOTS) {
    await page.screenshot({ path: `${SHOTS}/020-375-filtered.png` });
    await page.setViewportSize({ width: 1280, height: 720 });
  }

  await page.getByRole("checkbox", { name: `Select: ${SEED.watch}: v3.0.0 published` }).click();
  await page.getByRole("button", { name: "Select all 2" }).click();
  await page.getByRole("button", { name: "Mark read" }).click();
  // The chip keeps its name from the watch once the Unread tab is empty.
  await expect(page.getByRole("region", { name: `Nothing from ${SEED.watch} here` })).toBeVisible();
  expect(readIds()).toEqual([3, 1]);
  await expect(chip).toBeVisible();
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/020-1280-empty.png` });

  await chip.click();
  await expect(page).toHaveURL(/\/inbox$/);
  await expect(rows).toHaveCount(1);
  await expect(rows.first()).toContainText(other.name);

  // 020-AC10: at 375 px the link's padding takes taps (24 px hit box, WCAG 2.5.8), and the rest of the
  // row still opens the item.
  await page.setViewportSize({ width: 375, height: 812 });
  const link = rows.first().getByRole("link", { name: `Show only ${other.name}` });
  expect((await link.boundingBox())?.height).toBeGreaterThanOrEqual(24);
  // Above the glyphs, in the padding.
  await link.tap({ position: { x: 4, y: 2 } });
  await expect(page).toHaveURL(new RegExp(`/inbox\\?watch=${other.id}$`));
  // The unfiltered list also had one row: wait for the filtered one before measuring it.
  await expect.poll(() => asked.at(-1)?.searchParams.get("watch")).toBe(other.id);
  await expect(rows).toHaveCount(1);
  const row = await rows.first().boundingBox();
  const name = await link.boundingBox();
  if (!row || !name) throw new Error("row not laid out");
  await rows.first().tap({
    position: { x: name.x - row.x + name.width + 16, y: name.y - row.y + name.height / 2 },
  });
  const dialog = page.getByRole("dialog", { name: "v2.0.0 published" });
  await expect(dialog).toBeVisible();
  await expect(page).toHaveURL(new RegExp(`/inbox\\?watch=${other.id}$`));

  // 020-AC15: the dialog carries the action, as the hover group is out of reach on touch.
  const dialogAction = dialog.getByRole("button", { name: `Mark all from ${other.name} read` });
  await expect(dialogAction).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(375);
  // The dialog clips what overflows it: the button itself must fit, its long name truncated.
  const actionBox = await dialogAction.boundingBox();
  const dialogBox = await dialog.boundingBox();
  if (!actionBox || !dialogBox) throw new Error("dialog not laid out");
  expect(actionBox.x + actionBox.width).toBeLessThanOrEqual(dialogBox.x + dialogBox.width - 16);
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/020-375-dialog-action.png` });
  await dialogAction.tap();
  await expect(page.getByText(`1 item from ${other.name} marked read`)).toBeVisible();
  expect(watchReads).toEqual([`/api/v1/inbox/watches/${other.id}/read`]);
});

// 020-AC14: with a pointer, three quick actions on hover, inside the row and pinned to its right edge;
// the third reads the whole watch, opens nothing and selects nothing.
test.describe("with a pointer", () => {
  test.use({ hasTouch: false });
  test("mark all from a watch read from the row", async ({ page }) => {
    const { other, watchReads } = await serveInbox(page);
    const rows = page.getByRole("list", { name: /^Inbox, / }).getByRole("listitem");
    await expect(rows).toHaveCount(3);
    await rows.nth(1).hover();
    const quick = page.getByRole("group", { name: `Actions: ${other.name}: v2.0.0 published` });
    await expect(quick.getByRole("button")).toHaveCount(3);
    const readAll = quick.getByRole("button", { name: `Mark all from ${other.name} read` });
    await expect(readAll).toBeVisible();
    await expect(readAll).toHaveAttribute("title", `Mark all from ${other.name} read`);
    const rowBox = await rows.nth(1).boundingBox();
    const quickBox = await quick.boundingBox();
    if (!rowBox || !quickBox) throw new Error("row not laid out");
    expect(quickBox.x).toBeGreaterThan(rowBox.x);
    expect(Math.abs(rowBox.x + rowBox.width - (quickBox.x + quickBox.width) - 8)).toBeLessThan(2);
    if (SHOTS) await page.screenshot({ path: `${SHOTS}/020-1280-row-actions.png` });
    await readAll.click();
    await expect(page.getByText(`1 item from ${other.name} marked read`)).toBeVisible();
    expect(watchReads).toEqual([`/api/v1/inbox/watches/${other.id}/read`]);
    // Neither opens the item nor selects it.
    await expect(page.getByRole("complementary", { name: /published/ })).toHaveCount(0);
    await expect(page.getByRole("toolbar", { name: "Bulk actions" })).toHaveCount(0);
    if (SHOTS) await page.screenshot({ path: `${SHOTS}/020-1280-toast.png` });

    // At 1024 the open panel narrows the list: the row keeps its text, the panel carries the action.
    await page.setViewportSize({ width: 1024, height: 720 });
    await rows
      .first()
      .getByRole("button", { name: /v3\.0\.0 published/ })
      .click();
    const panel = page.getByRole("complementary", { name: "v3.0.0 published" });
    await expect(panel).toBeVisible();
    await rows.nth(1).hover();
    await expect(quick.getByRole("button", { name: "Mark as read" })).toBeVisible();
    await expect(readAll).toBeHidden();
    await expect(panel.getByRole("button", { name: /^Mark all from / })).toBeVisible();
    if (SHOTS) await page.screenshot({ path: `${SHOTS}/020-1024-panel-hover.png` });
  });
});

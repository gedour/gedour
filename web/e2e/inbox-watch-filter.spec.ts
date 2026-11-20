import { BOB_AUTH_FILE } from "./auth-file";
import { api, expect, SEED, test } from "./fixtures";

test.use({ storageState: BOB_AUTH_FILE, hasTouch: true });

const SHOTS = process.env.E2E_SHOTS_DIR;

// 020-AC5, 020-AC6, 020-AC7, 020-AC8, 020-AC10: filter by a watch, reload, select all, mark read, remove
// the chip, then tap the link and the row at 375 px.
// The list and the read call are served here: a bulk read of bob's shared inbox would race the
// keyboard spec's read toggles.
test("filter the inbox by a watch and mark its items read", async ({ page }) => {
  await page.goto("/inbox");
  const watches = (await (await (await api(page))("GET", "/api/v1/watches")).json()).items as {
    id: string;
    name: string;
    kind: string;
  }[];
  const seeded = watches.find((w) => w.name === SEED.watch);
  if (!seeded) throw new Error("seed.setup.ts did not run");
  const other = { id: "00000000-0000-4000-8000-000000000020", name: "Other watch", kind: "rss" };
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
  await page.reload();

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
  expect(readIds).toEqual([3, 1]);
  await expect(chip).toBeVisible();
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/020-1280-empty.png` });

  await chip.click();
  await expect(page).toHaveURL(/\/inbox$/);
  await expect(rows).toHaveCount(1);
  await expect(rows.first()).toContainText("Other watch");

  // 020-AC10: at 375 px the link's padding takes taps (24 px hit box, WCAG 2.5.8), and the rest of the
  // row still opens the item.
  await page.setViewportSize({ width: 375, height: 812 });
  const link = rows.first().getByRole("link", { name: "Show only Other watch" });
  expect((await link.boundingBox())?.height).toBeGreaterThanOrEqual(24);
  // Above the glyphs, in the padding.
  await link.tap({ position: { x: 4, y: 2 } });
  await expect(page).toHaveURL(new RegExp(`/inbox\\?watch=${other.id}$`));
  await expect(rows).toHaveCount(1);
  const row = await rows.first().boundingBox();
  const name = await link.boundingBox();
  if (!row || !name) throw new Error("row not laid out");
  await rows.first().tap({
    position: { x: name.x - row.x + name.width + 16, y: name.y - row.y + name.height / 2 },
  });
  await expect(page.getByRole("dialog", { name: "v2.0.0 published" })).toBeVisible();
  await expect(page).toHaveURL(new RegExp(`/inbox\\?watch=${other.id}$`));
});

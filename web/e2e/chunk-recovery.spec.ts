import type { Page } from "@playwright/test";

import { expect, test } from "./fixtures";

const CHUNK = "**/assets/ScriptFormPage-*.js";
// Past every retry delay of lib/chunks.ts.
const LONG_AFTER = 60_000;

function countDocuments(page: Page) {
  const docs = { count: 0 };
  page.on("request", (r) => {
    if (r.isNavigationRequest() && r.frame() === page.mainFrame()) docs.count++;
  });
  return docs;
}

/** Answers like a pod of another version (pkg/webui) while `gone` says so. */
async function missing(page: Page, gone: () => boolean = () => true) {
  await page.route(CHUNK, (route) =>
    gone()
      ? route.fulfill({ status: 404, contentType: "text/plain", body: "404 page not found\n" })
      : route.continue(),
  );
}

const reloads = (page: Page) => page.evaluate(() => sessionStorage.getItem("gedour.chunkReloads"));
const message = (page: Page) => page.getByRole("alert");
const expectMessage = async (page: Page) => {
  await expect(message(page)).toContainText("This page could not be loaded.");
  await expect(message(page).getByRole("button", { name: "Reload" })).toBeVisible();
  await expect(page.getByText("Unexpected Application Error")).toHaveCount(0);
  await expect(page.getByRole("navigation", { name: "Main" })).toBeVisible();
};

/** Moves the clock, then goes through the network: a reload the jump started would abort this fetch. */
async function expectNoReload(page: Page, docs: { count: number }, want: number) {
  await page.clock.fastForward(LONG_AFTER);
  await page.evaluate(() => fetch("/healthz").then(() => undefined));
  expect(docs.count).toBe(want);
}

const scriptName = (page: Page) => page.getByLabel("Name");

// The clock keeps running, unlike asset-recovery.spec.ts: this page needs the data layer's timers to
// render, so the exact delays are checked by RouteError.test.tsx and here only their end.
test.describe("lazy chunk lost during a rollout", () => {
  test.beforeEach(async ({ page }) => {
    await page.clock.install();
  });

  test("a chunk 404 on a fresh load reloads and the page opens", async ({ page }) => {
    const docs = countDocuments(page);
    await missing(page, () => docs.count < 2);
    await page.goto("/scripts/new");
    await expectMessage(page);

    await page.clock.fastForward(LONG_AFTER);
    await expect(scriptName(page)).toBeVisible();
    expect(docs.count).toBe(2);
    expect(await reloads(page)).toBeNull();
    await expectNoReload(page, docs, 2);
  });

  test("a permanent 404 stops at the limit and keeps the message", async ({ page }) => {
    const docs = countDocuments(page);
    await missing(page);
    await page.goto("/scripts/new");

    for (const tries of [1, 2, 3, 4]) {
      await expectMessage(page);
      await page.clock.fastForward(LONG_AFTER);
      await expect.poll(() => docs.count).toBe(tries + 1);
    }
    await expectMessage(page);
    expect(await reloads(page)).toBe("4");
    await expectNoReload(page, docs, 5);
    await expectMessage(page);

    // The button works past the limit, and a healed chunk clears the counter.
    await page.unroute(CHUNK);
    await page.getByRole("button", { name: "Reload" }).click();
    await expect(scriptName(page)).toBeVisible();
    expect(docs.count).toBe(6);
    expect(await reloads(page)).toBeNull();
  });

  test("the asset and chunk retries together stay bounded", async ({ page }) => {
    const docs = countDocuments(page);
    await missing(page);
    // Every other document also loses its CSS, which spends and resets index.html's own counter.
    await page.route("**/assets/*.css", (route) =>
      docs.count % 2 === 1
        ? route.fulfill({ status: 404, contentType: "text/plain", body: "404 page not found\n" })
        : route.continue(),
    );
    await page.goto("/scripts/new");

    await expect(async () => {
      const before = docs.count;
      await page.clock.fastForward(LONG_AFTER);
      await page.evaluate(() => fetch("/healthz").then(() => undefined));
      expect(docs.count).toBe(before);
      expect(await reloads(page)).toBe("4");
    }).toPass();
    await expectMessage(page);
    // First document, then at worst 4 chunk reloads each followed by 4 asset reloads and a clean one.
    expect(docs.count).toBeLessThanOrEqual(25);
  });

  test("a tab left open reloads on the navigation to the lost chunk", async ({ page }) => {
    const docs = countDocuments(page);
    await page.goto("/scripts");
    await expect(page.getByRole("link", { name: "New script" }).first()).toBeVisible();

    let gone = true;
    await missing(page, () => gone);
    await page.getByRole("link", { name: "New script" }).first().click();
    await expectMessage(page);
    await expect(page).toHaveURL(/\/scripts\/new$/);
    expect(docs.count).toBe(1);

    // The new pod serves the page again: the reload lands on the new index and its chunks.
    gone = false;
    await page.clock.fastForward(LONG_AFTER);
    await expect(scriptName(page)).toBeVisible();
    expect(docs.count).toBe(2);
  });

  test("leaving the failed page cancels the reload: a form typed meanwhile stays", async ({
    page,
  }) => {
    const docs = countDocuments(page);
    await missing(page);
    await page.goto("/scripts/new");
    await expectMessage(page);

    await page.getByRole("link", { name: "Rules", exact: true }).click();
    await page.getByRole("link", { name: "New rule" }).first().click();
    await page.getByLabel("Name").fill("Unsaved rule");

    await expectNoReload(page, docs, 1);
    await expect(page.getByLabel("Name")).toHaveValue("Unsaved rule");
    expect(await reloads(page)).toBeNull();
  });

  test("a dirty script form survives the chunk disappearing", async ({ page }) => {
    const docs = countDocuments(page);
    await page.goto("/scripts/new");
    await scriptName(page).fill("Unsaved draft");

    await missing(page);
    await expectNoReload(page, docs, 1);
    await expect(scriptName(page)).toHaveValue("Unsaved draft");
  });

  test("offline, the message stays instead of the browser's error page", async ({
    page,
    context,
  }) => {
    const docs = countDocuments(page);
    await page.goto("/scripts");
    await expect(page.getByRole("link", { name: "New script" }).first()).toBeVisible();

    await context.setOffline(true);
    await page.getByRole("link", { name: "New script" }).first().click();
    await expectMessage(page);
    await page.clock.fastForward(LONG_AFTER);
    await expectMessage(page);
    expect(docs.count).toBe(1);
    expect(await reloads(page)).toBeNull();
  });

  test("blocked sessionStorage shows the message without reloading", async ({ page }) => {
    const docs = countDocuments(page);
    await page.addInitScript(() => {
      Object.defineProperty(window, "sessionStorage", {
        get() {
          throw new DOMException("denied", "SecurityError");
        },
      });
    });
    await missing(page);
    await page.goto("/scripts/new");
    await expectMessage(page);
    await expectNoReload(page, docs, 1);
  });
});

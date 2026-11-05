import type { Page } from "@playwright/test";

import { expect, test } from "./fixtures";

// Past every retry delay of the script in index.html.
const LONG_AFTER = 60_000;

/** Answers like a pod of another version (pkg/webui) to the first `times` requests matching glob. */
async function missing(page: Page, glob: string, times = Infinity) {
  let left = times;
  await page.route(glob, (route) =>
    left-- > 0
      ? route.fulfill({ status: 404, contentType: "text/plain", body: "404 page not found\n" })
      : route.continue(),
  );
}

function countDocuments(page: Page) {
  const docs = { count: 0 };
  page.on("request", (r) => {
    if (r.isNavigationRequest() && r.frame() === page.mainFrame()) docs.count++;
  });
  return docs;
}

const reloads = (page: Page) => page.evaluate(() => sessionStorage.getItem("gedour.assetReloads"));

// The script stores the counter and arms its timer in one synchronous handler.
async function expectReloads(page: Page, want: string | null) {
  await expect(async () => {
    expect(await page.evaluate(() => document.readyState)).toBe("complete");
    expect(await reloads(page)).toBe(want);
  }).toPass();
}

/** Moves the clock, then goes through the network: a reload the jump started would abort this fetch. */
async function expectNoReload(page: Page, docs: { count: number }, want: number, ms = LONG_AFTER) {
  await page.clock.fastForward(ms);
  await page.evaluate(() => fetch("/healthz").then(() => undefined));
  expect(docs.count).toBe(want);
}

const sidebar = (page: Page) => page.getByRole("complementary", { name: "Sidebar" });

// Tailwind's preflight zeroes it; the browser default is 8px.
const body = (page: Page) => page.locator("body");

test.describe("hashed asset lost during a rollout", () => {
  test.beforeEach(async ({ page }) => {
    const now = Date.now();
    await page.clock.install({ time: now });
    await page.clock.pauseAt(now + 1000);
  });

  test("a CSS 404 reloads after 2 s and the style applies", async ({ page }) => {
    const docs = countDocuments(page);
    await missing(page, "**/assets/*.css", 1);
    await page.goto("/");
    await expectReloads(page, "1");
    await expect(body(page)).toHaveCSS("margin", "8px");

    await expectNoReload(page, docs, 1, 1999);
    await page.clock.fastForward(1);
    await expect.poll(() => docs.count).toBe(2);

    await expect(body(page)).toHaveCSS("margin", "0px");
    await expectReloads(page, null);
    await expectNoReload(page, docs, 2);
  });

  test("a JS 404 reloads and the app starts", async ({ page }) => {
    const docs = countDocuments(page);
    await missing(page, "**/assets/*.js", 1);
    await page.goto("/");
    await expectReloads(page, "1");
    await expect(page.locator("#root")).toBeEmpty();

    await page.clock.fastForward(2000);
    await expect.poll(() => docs.count).toBe(2);

    await expectReloads(page, null);
    await page.clock.resume(); // The data layer flushes through timers.
    await expect(sidebar(page)).toBeVisible();
  });

  test("CSS and JS lost on the same load cost one try", async ({ page }) => {
    const docs = countDocuments(page);
    await missing(page, "**/assets/*.css", 1);
    await missing(page, "**/assets/*.js", 1);
    await page.goto("/");
    await expectReloads(page, "1");

    await page.clock.fastForward(2000);
    await expect.poll(() => docs.count).toBe(2);

    await expect(body(page)).toHaveCSS("margin", "0px");
    await expectReloads(page, null);
  });

  test("a permanent 404 stops after 4 tries at 2, 4, 8 and 16 s", async ({ page }) => {
    const docs = countDocuments(page);
    await missing(page, "**/assets/*.css");
    await page.goto("/");

    for (const [tries, delay] of [
      [1, 2000],
      [2, 4000],
      [3, 8000],
      [4, 16000],
    ] as const) {
      await expectReloads(page, String(tries));
      expect(docs.count).toBe(tries);
      await expectNoReload(page, docs, tries, delay - 1);
      await page.clock.fastForward(1);
      await expect.poll(() => docs.count).toBe(tries + 1);
    }

    await expectReloads(page, "4");
    await expectNoReload(page, docs, 5);
    await expect(body(page)).toHaveCSS("margin", "8px");
  });

  test("an asset failing after load leaves the page alone", async ({ page }) => {
    const docs = countDocuments(page);
    await page.goto("/");
    await page.evaluate(
      () =>
        new Promise((resolve) => {
          const link = document.createElement("link");
          link.rel = "stylesheet";
          link.href = "/assets/gone.css";
          link.onerror = resolve;
          document.head.append(link);
        }),
    );
    await expectReloads(page, null);
    await expectNoReload(page, docs, 1);
  });

  test("other failures before load do not count", async ({ page }) => {
    const docs = countDocuments(page);
    await missing(page, "**/other/gone.css");
    await page.addInitScript(() => {
      document.addEventListener("DOMContentLoaded", () => {
        const link = document.createElement("link");
        link.rel = "stylesheet";
        link.href = "/other/gone.css";
        const img = document.createElement("img");
        img.src = "/assets/gone.png";
        document.head.append(link, img);
      });
    });
    const failed = page.waitForResponse("**/other/gone.css");
    const image = page.waitForResponse("**/assets/gone.png");
    await page.goto("/");
    await Promise.all([failed, image]);
    await expectReloads(page, null);
    await expectNoReload(page, docs, 1);
  });

  test("blocked sessionStorage means no reload and no error", async ({ page }) => {
    const docs = countDocuments(page);
    const errors: string[] = [];
    page.on("pageerror", (e) => errors.push(e.message));
    await page.addInitScript(() => {
      Object.defineProperty(window, "sessionStorage", {
        get() {
          throw new DOMException("denied", "SecurityError");
        },
      });
    });
    await missing(page, "**/assets/*.css");
    await page.goto("/");
    await expectNoReload(page, docs, 1);
    expect(errors).toEqual([]);
  });
});

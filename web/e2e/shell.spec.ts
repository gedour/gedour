import type { Page } from "@playwright/test";

import { expect, test } from "./fixtures";

const sidebar = (page: Page) => page.getByRole("complementary", { name: "Sidebar" });

async function sidebarWidth(page: Page): Promise<number> {
  const box = await sidebar(page).boundingBox();
  return Math.round(box?.width ?? 0);
}

async function dragHandleBy(page: Page, dx: number) {
  const box = await page.getByRole("separator", { name: "Resize sidebar" }).boundingBox();
  if (!box) throw new Error("resize handle not visible");
  const x = box.x + box.width / 2;
  const y = box.y + box.height / 2;
  await page.mouse.move(x, y);
  await page.mouse.down();
  await page.mouse.move(x + dx / 2, y, { steps: 5 });
  await page.mouse.move(x + dx, y, { steps: 5 });
  await page.mouse.up();
}

// 001-AC27: /healthz answers 200 with PostgreSQL up.
test("healthz is 200", async ({ request }) => {
  const res = await request.get("/healthz");
  expect(res.status()).toBe(200);
  expect(await res.json()).toEqual({ status: "ok" });
});

test("shell renders with navigation and server version", async ({ page }) => {
  const res = await page.goto("/");
  expect(res?.headers()["content-security-policy"]).toMatch(/script-src 'self' 'sha256-[^']+'/);
  await expect(page).toHaveURL(/\/inbox$/);
  await expect(page.getByRole("heading", { level: 1, name: "Inbox" })).toBeVisible();
  // Alice is admin: the 8 sections plus Administration (006-AC5).
  await expect(page.getByRole("navigation", { name: "Main" }).getByRole("link")).toHaveCount(9);
  await expect(page.getByTestId("app-version")).toHaveText(/^v\d+\.\d+\.\d+.* \([0-9a-f]+\)$/);

  // A client route survives a hard reload through the server's SPA fallback.
  await page.getByRole("link", { name: "Audit" }).click();
  await page.reload();
  await expect(page.getByRole("heading", { level: 1, name: "Audit" })).toBeVisible();
});

// 001-AC23: collapse by button and shortcut, state survives a reload.
test("sidebar collapse persists across reload", async ({ page }) => {
  await page.goto("/inbox");
  await page.getByRole("button", { name: "Collapse sidebar" }).click();
  await expect(sidebar(page)).toHaveAttribute("data-collapsed", "true");
  await expect.poll(() => sidebarWidth(page)).toBe(64);

  await page.reload();
  await expect(sidebar(page)).toHaveAttribute("data-collapsed", "true");
  await expect(page.getByRole("button", { name: "Expand sidebar" })).toBeVisible();

  await page.keyboard.press("ControlOrMeta+b");
  await expect(sidebar(page)).toHaveAttribute("data-collapsed", "false");
  await page.reload();
  await expect(sidebar(page)).toHaveAttribute("data-collapsed", "false");
});

// 001-AC23: drag resize clamps to 180..480 px and the width survives a reload.
test("sidebar resize clamps and persists", async ({ page }) => {
  await page.goto("/inbox");
  await dragHandleBy(page, 1000);
  await expect.poll(() => sidebarWidth(page)).toBe(480);
  await page.reload();
  await expect.poll(() => sidebarWidth(page)).toBe(480);

  await dragHandleBy(page, -1000);
  await expect.poll(() => sidebarWidth(page)).toBe(180);
  await page.reload();
  await expect.poll(() => sidebarWidth(page)).toBe(180);
});

// 001-AC25: the theme follows the OS until forced, and a forced choice survives a reload.
test("theme follows the OS and a forced choice persists", async ({ page }) => {
  await page.emulateMedia({ colorScheme: "dark" });
  await page.goto("/inbox");
  await expect(page.locator("html")).toHaveClass(/\bdark\b/);

  await page.emulateMedia({ colorScheme: "light" });
  await expect(page.locator("html")).not.toHaveClass(/\bdark\b/);

  await page.getByRole("button", { name: /^Theme:/ }).click();
  await page.getByRole("menuitemradio", { name: "Dark" }).click();
  await expect(page.locator("html")).toHaveClass(/\bdark\b/);

  await page.reload();
  await expect(page.locator("html")).toHaveClass(/\bdark\b/);
  await expect(page.getByRole("button", { name: "Theme: dark" })).toBeVisible();
});

// Every interactive overlay renders under the CSP: theme menu, collapsed-nav tooltip, mobile drawer.
test("menus, tooltips and the drawer open without CSP violations", async ({ page }) => {
  await page.goto("/inbox");
  await page.getByRole("button", { name: /^Theme:/ }).click();
  await expect(page.getByRole("menuitemradio", { name: "Dark" })).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.getByRole("menu")).toBeHidden();

  await page.getByRole("button", { name: "Collapse sidebar" }).click();
  // Hovering during the width transition would leave the pointer past the link once it ends.
  await expect(sidebar(page)).toHaveCSS("width", "64px");
  await page.getByRole("link", { name: "Watches" }).hover();
  await expect(page.getByRole("tooltip", { name: "Watches" })).toBeVisible();

  await page.setViewportSize({ width: 375, height: 760 });
  await page.getByRole("button", { name: "Open navigation" }).click();
  await expect(sidebar(page)).not.toHaveAttribute("inert");
  await page.getByRole("link", { name: "Rules" }).click();
  await expect(page.getByRole("heading", { level: 1, name: "Rules" })).toBeVisible();
  await expect(sidebar(page)).toHaveAttribute("inert");
  await page.getByRole("button", { name: /^Theme:/ }).click();
  await expect(page.getByRole("menuitemradio", { name: "Light" })).toBeVisible();
});

// The sticky bar covers the document's top edge, by a height that depends on the act-as banner.
const barBottom = (page: Page) =>
  page.evaluate(
    () => document.querySelector("header")?.parentElement?.getBoundingClientRect().bottom,
  );

/** Serves the shell acting or not, with 40 inbox rows and 40 watches so the document scrolls. */
async function longLists(page: Page, acting: boolean) {
  await page.route("**/api/v1/me", async (route) => {
    const res = await route.fetch();
    const me = (await res.json()) as object;
    const acting_as = acting ? { id: "x", email: "b@example.test", display_name: "B" } : null;
    await route.fulfill({ response: res, json: { ...me, acting_as } });
  });
  const stamp = new Date().toISOString();
  const watchRef = { id: "w", name: "Long list", kind: "github_repo" };
  await page.route(/\/api\/v1\/inbox\?/, (route) =>
    route.fulfill({
      json: {
        items: Array.from({ length: 40 }, (_, i) => ({
          id: i + 1,
          created_at: stamp,
          read_at: stamp,
          archived_at: null,
          priority: "normal",
          event: {
            id: i + 1,
            type: "release",
            occurred_at: stamp,
            observed_at: stamp,
            payload: { tag: `v${i}` },
          },
          watch: watchRef,
        })),
      },
    }),
  );
  await page.route(/\/api\/v1\/watches(\?|$)/, (route) =>
    route.fulfill({
      json: {
        items: Array.from({ length: 40 }, (_, i) => ({
          id: `w${i}`,
          name: `Long ${i}`,
          kind: "github_repo",
          config: {
            repo: `o/r${i}`,
            include_prereleases: false,
            api_url: "https://api.github.com",
          },
          credential_id: null,
          schedule: "15m",
          paused: false,
          owner_id: "x",
          owner_name: "A",
          permission: "owner",
          created_at: stamp,
          updated_at: stamp,
          next_run_at: stamp,
          last_run_at: null,
          last_status: null,
          last_error: null,
          consecutive_failures: 0,
          running: false,
        })),
      },
    }),
  );
}

/** The focused element's top against the bar's bottom; positive when it clears the bar. */
const focusClearance = (page: Page) =>
  page.evaluate(() => {
    const bar =
      document.querySelector("header")?.parentElement?.getBoundingClientRect().bottom ?? 0;
    return (document.activeElement?.getBoundingClientRect().top ?? 0) - bar;
  });

test("the skip link shows on top of the bar and moves focus to the content", async ({ page }) => {
  await page.goto("/watches");
  await expect(page.getByRole("heading", { level: 1, name: "Watches" })).toBeVisible();
  await page.keyboard.press("Tab");
  const link = page.getByRole("link", { name: "Skip to content" });
  await expect(link).toBeFocused();
  const onTop = await link.evaluate((el) => {
    const r = el.getBoundingClientRect();
    return document.elementFromPoint(r.x + r.width / 2, r.y + r.height / 2) === el;
  });
  expect(onTop).toBe(true);

  await page.keyboard.press("Enter");
  await expect(page.locator("#main")).toBeFocused();
  const h1 = await page.getByRole("heading", { level: 1 }).boundingBox();
  expect(h1?.y).toBeGreaterThanOrEqual((await barBottom(page)) ?? Infinity);
});

for (const [width, height] of [
  [1280, 800],
  [375, 760],
] as const) {
  for (const acting of [false, true]) {
    test(`focus clears the sticky bar at ${width} px, ${acting ? "acting" : "not acting"}`, async ({
      page,
    }) => {
      await page.setViewportSize({ width, height });
      await longLists(page, acting);

      await page.goto("/inbox");
      await expect(page.locator("#inbox-item-40")).toBeAttached();
      expect(await barBottom(page)).toBe(acting ? 100 : 56);
      for (let i = 0; i < 39; i++) await page.keyboard.press("j");
      await expect(page.locator("#inbox-item-40")).toBeFocused();
      for (let i = 0; i < 20; i++) {
        await page.keyboard.press("k");
        expect(await focusClearance(page)).toBeGreaterThanOrEqual(0);
      }

      await page.goto("/watches");
      const rows = page.getByRole("list", { name: "Watches" }).getByRole("link");
      await expect(rows).toHaveCount(40);
      await rows.last().focus();
      for (let i = 0; i < 40; i++) {
        await page.keyboard.press("Shift+Tab");
        expect(await focusClearance(page)).toBeGreaterThanOrEqual(0);
      }

      // The bar and the sidebar are sticky: focus inside them leaves the page where it is.
      await page.evaluate(() => window.scrollTo(0, 400));
      await page.getByRole("button", { name: /^Theme:/ }).focus();
      expect(await page.evaluate(() => window.scrollY)).toBe(400);
    });
  }
}

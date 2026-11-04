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
  await expect(page.getByRole("navigation", { name: "Main" }).getByRole("link")).toHaveCount(7);
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

import type { Locator, Page } from "@playwright/test";

import { BOB_AUTH_FILE } from "./auth-file";
import { expect, test, UPSTREAM } from "./fixtures";

test.use({ storageState: BOB_AUTH_FILE });

const SHOTS = process.env.E2E_SHOTS_DIR;
const tag = () => `${Date.now().toString(36)}${Math.floor(Math.random() * 1e4)}`;

/** Asserts target has keyboard focus with a visible indicator: its own outline or its card's ring. */
async function expectFocusVisible(target: Locator) {
  const state = await target.evaluate((el) => {
    const s = getComputedStyle(el);
    const outline = s.outlineStyle !== "none" && parseFloat(s.outlineWidth) > 0;
    // A visually hidden radio shows a ring (box-shadow) on the card around it instead.
    const card = el.matches(".sr-only") ? el.closest("label") : null;
    return {
      focusVisible: el.matches(":focus-visible"),
      indicator: outline || (card !== null && getComputedStyle(card).boxShadow !== "none"),
    };
  });
  expect(state).toEqual({ focusVisible: true, indicator: true });
}

/** Presses Tab (or Shift+Tab) until target has focus, then checks the focus is visible. */
async function tabTo(page: Page, target: Locator, back = false) {
  await expect(target).toBeAttached();
  for (let i = 0; i < 80; i++) {
    await page.keyboard.press(back ? "Shift+Tab" : "Tab");
    if (await target.evaluate((el) => el === document.activeElement)) {
      await expectFocusVisible(target);
      return;
    }
  }
  throw new Error(`Tab never reached ${target.toString()}`);
}

const nav = (page: Page, name: string | RegExp) =>
  page.getByRole("navigation", { name: "Main" }).getByRole("link", { name });

// 001-AC26: a watch, a rule and a read inbox item with the keyboard alone, focus always visible.
test("create a watch and a rule, then read an inbox item, by keyboard only", async ({ page }) => {
  const t = tag();
  const name = `Kbd ${t}`;
  await page.goto("/watches");
  await expect(page.getByRole("heading", { level: 1, name: "Watches" })).toBeVisible();

  await tabTo(page, page.getByRole("link", { name: "New watch" }).first());
  await page.keyboard.press("Enter");
  await expect(page.getByRole("heading", { level: 1, name: "New watch" })).toBeVisible();
  await tabTo(page, page.getByLabel("Paste a URL"));
  await page.keyboard.type("https://github.com/cli/cli/releases");
  await expect(page.getByText("Recognized: GitHub releases")).toBeVisible();
  await tabTo(page, page.getByLabel("API URL (optional)"));
  await page.keyboard.type(UPSTREAM);
  await tabTo(page, page.getByLabel("Name"));
  await page.keyboard.type(name);
  const create = page.getByRole("button", { name: "Create watch" });
  await tabTo(page, create);
  if (SHOTS) {
    // transition-colors animates outline-color: let the ring reach its colour first.
    await page.waitForTimeout(300);
    await page.screenshot({ path: `${SHOTS}/phase7-keyboard-focus.png` });
  }
  await page.keyboard.press("Enter");
  await expect(page.getByRole("heading", { level: 1, name })).toBeVisible();

  await tabTo(page, nav(page, "Rules"), true);
  await page.keyboard.press("Enter");
  await expect(page.getByRole("heading", { level: 1, name: "Rules" })).toBeVisible();
  await tabTo(page, page.getByRole("link", { name: "New rule" }).first());
  await page.keyboard.press("Enter");
  await expect(page.getByRole("heading", { level: 1, name: "New rule" })).toBeVisible();
  await tabTo(page, page.getByLabel("Name"));
  await page.keyboard.type(`Kbd rule ${t}`);
  const watches = page.getByLabel("Watches");
  await tabTo(page, watches);
  // A closed native select picks the option whose label starts with what is typed.
  await page.keyboard.type(name);
  await expect(watches.locator("option:checked")).toHaveText(name);
  await tabTo(page, page.getByLabel("Expression"));
  await page.keyboard.type('event.type == "release.published"');
  await expect(page.getByText("Valid filter")).toBeVisible();
  await tabTo(page, page.getByRole("button", { name: "Create rule" }));
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(/\/rules$/);
  await expect(page.getByRole("list", { name: "Rules" })).toContainText(`Kbd rule ${t}`);

  // The seeded rc promotion: the other specs open the first (newest) item, never this one.
  await tabTo(page, nav(page, /^Inbox/), true);
  await page.keyboard.press("Enter");
  await expect(page.getByRole("heading", { level: 1, name: "Inbox" })).toBeVisible();
  const list = page.getByRole("list", { name: /^Inbox, / });
  const button = page.getByRole("button", { name: /promoted to release/ });
  const item = list.getByRole("listitem").filter({ has: button });
  const row = item.getByRole("button", { name: /promoted to release/ });
  await tabTo(page, row);
  if ((await item.getAttribute("data-unread")) === "false") {
    await page.keyboard.press("u");
    await expect(item).toHaveAttribute("data-unread", "true");
  }
  // Enter opens the detail, which marks it read; Escape closes it and gives focus back.
  await page.keyboard.press("Enter");
  await expect(page.getByRole("button", { name: "Close details" })).toBeVisible();
  await expect(item).toHaveAttribute("data-unread", "false");
  await page.keyboard.press("Escape");
  await expect(page.getByRole("button", { name: "Close details" })).toBeHidden();
  await expect(row).toBeFocused();
  // The u shortcut toggles it back and forth.
  await page.keyboard.press("u");
  await expect(item).toHaveAttribute("data-unread", "true");
  await page.keyboard.press("u");
  await expect(item).toHaveAttribute("data-unread", "false");
  await page.reload();
  await expect(item).toHaveAttribute("data-unread", "false");
});

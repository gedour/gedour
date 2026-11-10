import type { Locator, Page } from "@playwright/test";

import { BOB_AUTH_FILE } from "./auth-file";
import { addDevice, enterFilter, expect, gate, SEED, test, UPSTREAM } from "./fixtures";

test.use({ storageState: BOB_AUTH_FILE });

const SHOTS = process.env.E2E_SHOTS_DIR;
const tag = () => `${Date.now().toString(36)}${Math.floor(Math.random() * 1e4)}`;

/** Types a filter into the focused field, replacing what a previous attempt typed. */
const retype = (page: Page) => async (text: string, attempt: number) => {
  if (attempt > 0) await page.keyboard.press("ControlOrMeta+a");
  await page.keyboard.type(text);
};

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
  await expect(page.getByText("Recognized: GitHub repository")).toBeVisible();
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
  await enterFilter(page, 'event.type == "release.published"', retype(page));
  await tabTo(page, page.getByRole("button", { name: "Create rule" }));
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(/\/rules$/);
  await expect(page.getByRole("list", { name: "Rules" })).toContainText(`Kbd rule ${t}`);

  // The seeded rc promotion: the other specs open the first (newest) item, never this one.
  await tabTo(page, nav(page, /^Inbox/), true);
  await page.keyboard.press("Enter");
  await expect(page.getByRole("heading", { level: 1, name: "Inbox" })).toBeVisible();
  // The inbox opens on Unread; these rows include read ones.
  await tabTo(page, page.getByRole("button", { name: "All", exact: true }));
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(/state=all/);
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

// 005-AC7: the rule form's priority and the inbox filter from the keyboard, focus always visible.
test("set a rule's priority and filter the inbox, by keyboard only", async ({ page }) => {
  const t = tag();
  await page.goto("/rules/new");
  await tabTo(page, page.getByLabel("Name"));
  await page.keyboard.type(`Kbd high ${t}`);
  const watches = page.getByLabel("Watches");
  await tabTo(page, watches);
  await page.keyboard.type(SEED.watch);
  await expect(watches.locator("option:checked")).toHaveText(SEED.watch);
  const priority = page.getByLabel("Priority");
  await tabTo(page, priority);
  await page.keyboard.type("High");
  await expect(priority).toHaveValue("high");
  await tabTo(page, page.getByLabel("Expression"));
  // Matches nothing: bob's seeded items stay as they are.
  await enterFilter(page, 'event.type == "none"', retype(page));
  await tabTo(page, page.getByRole("button", { name: "Create rule" }));
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(/\/rules$/);
  await expect(
    page
      .getByRole("listitem")
      .filter({ hasText: `Kbd high ${t}` })
      .getByText("High priority"),
  ).toBeVisible();

  await tabTo(page, nav(page, /^Inbox/), true);
  await page.keyboard.press("Enter");
  await expect(page.getByRole("heading", { level: 1, name: "Inbox" })).toBeVisible();
  // The inbox opens on Unread; these rows include read ones.
  await tabTo(page, page.getByRole("button", { name: "All", exact: true }));
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(/state=all/);
  const filter = page.getByRole("button", { name: "High priority", exact: true });
  await tabTo(page, filter);
  await page.keyboard.press("Enter");
  await expect(filter).toHaveAttribute("aria-pressed", "true");
  await expect(page).toHaveURL(/priority=high/);
  const rows = page.getByRole("list", { name: /^Inbox, / }).getByRole("listitem");
  await expect(rows).toHaveCount(2);
  await page.keyboard.press("Space");
  await expect(filter).toHaveAttribute("aria-pressed", "false");
  await expect(rows).toHaveCount(3);
});

// 007-AC32: create, reveal and revoke an API key with the keyboard alone, focus always visible.
test("create and revoke an API key by keyboard only", async ({ page }) => {
  const name = `Kbd key ${tag()}`;
  await page.goto("/keys");
  await expect(page.getByRole("heading", { level: 1, name: "API keys" })).toBeVisible();
  await tabTo(page, page.getByRole("button", { name: "New API key" }));
  await page.keyboard.press("Enter");
  const dialog = page.getByRole("dialog", { name: "New API key" });
  await expect(dialog.getByLabel("Name", { exact: true })).toBeFocused();
  await page.keyboard.type(name);
  await tabTo(page, dialog.getByRole("radio", { name: "Read" }));
  await page.keyboard.press("ArrowDown");
  await expect(dialog.getByRole("radio", { name: "Write" })).toBeChecked();
  await tabTo(page, dialog.getByRole("button", { name: "Create key" }));
  await page.keyboard.press("Enter");
  const reveal = page.getByRole("dialog", { name: `Copy ${name} now` });
  await tabTo(page, reveal.getByRole("button", { name: "I have copied it" }));
  await page.keyboard.press("Enter");
  await expect(reveal).toBeHidden();
  const row = page.getByRole("row").filter({ hasText: name });
  await tabTo(page, row.getByRole("button", { name: "Revoke" }));
  await page.keyboard.press("Enter");
  const confirm = page.getByRole("dialog", { name: `Revoke ${name}?` });
  await tabTo(page, confirm.getByRole("button", { name: "Revoke" }));
  await page.keyboard.press("Enter");
  await expect(row).toContainText("revoked");
  // The row's Revoke button is gone: focus lands somewhere a keyboard user can go on from.
  await expect(page.getByRole("button", { name: "New API key" })).toBeFocused();
});

// 003-AC5: the Talk form's type radio and Silent switch work from the keyboard.
test("the Silent switch by keyboard", async ({ page }) => {
  await page.goto("/channels");
  const open = page.getByRole("button", { name: "New channel" }).first();
  await open.focus();
  await page.keyboard.press("Enter");
  const dialog = page.getByRole("dialog", { name: "New channel" });
  const webhook = dialog.getByRole("radio", { name: "Webhook" });
  await tabTo(page, webhook);
  await page.keyboard.press("ArrowLeft");
  await page.keyboard.press("ArrowLeft");
  await expect(dialog.getByRole("radio", { name: "Nextcloud Talk" })).toBeChecked();
  const silent = dialog.getByRole("switch", { name: "Silent" });
  await tabTo(page, silent);
  await page.keyboard.press("Space");
  await expect(silent).toBeChecked();
  await page.keyboard.press("Enter");
  await expect(silent).not.toBeChecked();
  await page.keyboard.press("Escape");
  await expect(dialog).toBeHidden();
});

// 010-AC15, 010-AC18: in the Web Push channel form, rename a device, pick it, then remove it, by keyboard only.
test("Web Push form and its devices by keyboard", async ({ page }) => {
  const device = await addDevice(page, "Keyboard");
  await page.goto("/channels");
  const open = page.getByRole("button", { name: "New channel" }).first();
  await open.focus();
  await page.keyboard.press("Enter");
  const form = page.getByRole("dialog", { name: "New channel" });
  await tabTo(page, form.getByRole("radio", { name: "Webhook" }));
  await page.keyboard.press("ArrowRight");
  await page.keyboard.press("ArrowRight");
  await expect(form.getByRole("radio", { name: "Web Push" })).toBeChecked();
  const devices = form.getByRole("list", { name: "My devices" });

  await tabTo(page, devices.getByRole("button", { name: `Rename ${device}` }));
  await page.keyboard.press("Enter");
  const dialog = page.getByRole("dialog", { name: `Rename ${device}` });
  await expect(dialog.getByLabel("Name", { exact: true })).toBeFocused();
  await page.keyboard.press("ControlOrMeta+a");
  await page.keyboard.type(`${device} 2`);
  await page.keyboard.press("Enter");
  await expect(dialog).toBeHidden();
  await expect(devices).toContainText(`${device} 2`);
  await expect(form).toBeVisible();

  await tabTo(page, form.getByRole("radio", { name: /All my devices/ }));
  await page.keyboard.press("ArrowDown");
  await expect(form.getByRole("radio", { name: /Pick devices/ })).toBeChecked();
  const box = devices.getByRole("checkbox", { name: `${device} 2` });
  await tabTo(page, box);
  await page.keyboard.press("Space");
  await expect(box).toBeChecked();
  const silent = form.getByRole("switch", { name: "Silent" });
  await tabTo(page, silent);
  await page.keyboard.press("Space");
  await expect(silent).toBeChecked();

  // Removal asks first; its confirm button by keyboard, then the focus stays in the form: on a neighbour
  // device, else on the devices group (this headless browser cannot be added).
  const renames = await devices
    .getByRole("button", { name: /^Rename / })
    .evaluateAll((els) => els.map((e) => e.getAttribute("aria-label") ?? ""));
  const at = renames.indexOf(`Rename ${device} 2`);
  const next = renames[at + 1] ?? renames[at - 1];
  await tabTo(page, devices.getByRole("button", { name: `Remove ${device} 2` }), true);
  await page.keyboard.press("Enter");
  const confirm = page.getByRole("dialog", { name: `Remove ${device} 2?` });
  await tabTo(page, confirm.getByRole("button", { name: "Remove" }));
  await page.keyboard.press("Enter");
  await expect(confirm).toBeHidden();
  await expect(devices.getByRole("button", { name: `Rename ${device} 2` })).toHaveCount(0);
  await expect(
    next
      ? devices.getByRole("button", { name: next, exact: true })
      : form.getByRole("group", { name: "Devices" }),
  ).toBeFocused();
  await expect(form).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(form).toBeHidden();
});

test.describe("without a session", () => {
  test.use({ storageState: { cookies: [], origins: [] } });

  // 002-AC8: the gate's only action is the first Tab stop, visibly focused, and Enter follows it.
  test("sign in from the gate by keyboard", async ({ page }) => {
    await page.goto("/rules");
    await expect(gate(page)).toBeVisible();
    const signIn = page.getByRole("link", { name: "Sign in" });
    await page.keyboard.press("Tab");
    await expect(signIn).toBeFocused();
    await expectFocusVisible(signIn);
    await page.keyboard.press("Enter");
    await expect(page).toHaveURL(/\/realms\/gedour-test\/protocol\/openid-connect\/auth/);
  });
});

// 002-AC8: the gate replacing the app takes the focus, which would otherwise fall to <body>.
test("focus moves to the gate when the session ends", async ({ page, context }) => {
  await page.goto("/inbox");
  await expect(page.getByRole("heading", { level: 1, name: "Inbox" })).toBeVisible();
  const rules = nav(page, "Rules");
  await tabTo(page, rules);
  await context.clearCookies();
  await page.keyboard.press("Enter");
  await expect(gate(page)).toBeFocused();
  await page.keyboard.press("Tab");
  await expect(page.getByRole("link", { name: "Sign in" })).toBeFocused();
});

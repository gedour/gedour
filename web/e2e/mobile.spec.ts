import type { Page } from "@playwright/test";

import { BOB_AUTH_FILE } from "./auth-file";
import { api, enterFilter, expect, gate, SEED, test, UPSTREAM } from "./fixtures";

test.use({ storageState: BOB_AUTH_FILE, viewport: { width: 375, height: 812 }, hasTouch: true });

const SHOTS = process.env.E2E_SHOTS_DIR;
const tag = () => `${Date.now().toString(36)}${Math.floor(Math.random() * 1e4)}`;

/** Fails when the page, or main, is wider than the viewport. */
async function expectNoHorizontalScroll(page: Page) {
  const widths = await page.evaluate(() =>
    [document.documentElement, document.getElementById("main")].flatMap((el) =>
      el ? [{ scroll: el.scrollWidth, client: el.clientWidth }] : [],
    ),
  );
  for (const w of widths) {
    expect(w.scroll, `${page.url()}: content wider than the viewport`).toBeLessThanOrEqual(
      w.client,
    );
  }
}

async function shot(page: Page, name: string) {
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/phase7-375-${name}.png` });
}

// 001-AC26: every main view fits 375 px without horizontal scroll.
test("main views fit 375 px", async ({ page }) => {
  await page.goto("/watches");
  const watches = (await (await (await api(page))("GET", "/api/v1/watches")).json()).items as {
    id: string;
    name: string;
  }[];
  const seeded = watches.find((w) => w.name === SEED.watch)?.id;
  expect(seeded, "seed.setup.ts did not run").toBeDefined();
  for (const path of [
    `/watches/${seeded}`,
    "/inbox?state=all",
    "/inbox?priority=high",
    "/watches",
    "/watches/new",
    "/rules",
    "/rules/new",
    "/channels",
    "/scripts",
    "/scripts/new",
    "/audit",
    "/settings",
    "/settings/credentials",
    "/keys",
  ]) {
    await page.goto(path);
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
    await expect(page.locator("[aria-busy=true]")).toHaveCount(0);
    // 008-AC15: the default kind's form shows the new-items boxes and their hint.
    // 011-AC17: and the discussions box with its own hint.
    if (path === "/watches/new") {
      await expect(page.getByText(/1 more request:/)).toBeVisible();
      await expect(page.getByText(/2 to 3 more requests/)).toBeVisible();
    }
    await expectNoHorizontalScroll(page);
  }
  await page.goto("/auth/signed-out");
  await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
  await expectNoHorizontalScroll(page);
});

// 007-AC32: the create form and the reveal, with its long lines, fit 375 px.
test("the API key dialogs fit 375 px", async ({ page }) => {
  const name = `Phone ${tag()}`;
  await page.goto("/keys");
  await page.getByRole("button", { name: "New API key" }).click();
  const dialog = page.getByRole("dialog", { name: "New API key" });
  const fits = (el: HTMLElement) => el.scrollWidth <= el.clientWidth;
  await dialog.getByLabel("Name", { exact: true }).fill(name);
  expect(await dialog.evaluate(fits)).toBe(true);
  await dialog.getByRole("button", { name: "Create key" }).click();
  const reveal = page.getByRole("dialog", { name: `Copy ${name} now` });
  await expect(reveal.getByTestId("api-key-reveal")).toBeVisible();
  expect(await reveal.evaluate(fits)).toBe(true);
  await reveal.getByRole("button", { name: "I have copied it" }).click();
  await expectNoHorizontalScroll(page);
  await page
    .getByRole("row")
    .filter({ hasText: name })
    .getByRole("button", { name: "Revoke" })
    .click();
  await page.getByRole("dialog").getByRole("button", { name: "Revoke" }).click();
  await expect(page.getByRole("row").filter({ hasText: name })).toContainText("revoked");
});

// 004-AC7: the panel at 375 px, with the longest notes and id list a release can carry: one unbreakable word.
test("the inbox panel fits 375 px with 64 KiB of notes", async ({ page }) => {
  await page.route("**/api/v1/inbox?*", async (route) => {
    const res = await route.fetch();
    const body = (await res.json()) as { items: { event: { payload: object } }[] };
    for (const it of body.items) {
      it.event.payload = {
        ...it.event.payload,
        body: "x".repeat(64 << 10),
        cves: Array.from({ length: 200 }, (_, i) => `CVE-2026-${10000 + i}`),
      };
    }
    await route.fulfill({ response: res, json: body });
  });
  await page.goto("/inbox?state=all");
  await page
    .getByRole("list", { name: /^Inbox, / })
    .getByRole("button")
    .first()
    .tap();
  const notes = page.getByText("Release notes").first();
  await expect(notes).toBeVisible();
  await expect(page.getByRole("region", { name: "Details" }).first()).toContainText(
    "CVE-2026-10199",
  );
  await expectNoHorizontalScroll(page);
  // The notes stay collapsed until asked for: the buttons below are a short scroll away.
  const closed = await page
    .locator("details")
    .first()
    .evaluate((el) => el.getBoundingClientRect().height);
  expect(closed).toBeLessThan(100);
  await notes.tap();
  await expect(page.locator("details[open] pre").first()).toBeVisible();
  const details = await page
    .locator("details[open]")
    .first()
    .evaluate((el) => ({ scroll: el.scrollWidth, client: el.clientWidth }));
  expect(details.scroll).toBeLessThanOrEqual(details.client);
  await expectNoHorizontalScroll(page);
});

// 005-AC7: the filter by touch at 375 px, the badge in the rows and the dialog, the form's control.
test("high priority at 375 px", async ({ page }) => {
  await page.goto("/inbox?state=all");
  const filter = page.getByRole("button", { name: "High priority", exact: true });
  await expect(filter).toBeInViewport();
  await expectNoHorizontalScroll(page);
  await filter.tap();
  await expect(page).toHaveURL(/priority=high/);
  const rows = page.getByRole("list", { name: /^Inbox, / }).getByRole("listitem");
  await expect(rows).toHaveCount(2);
  // Rows clip their overflow, which the page's scroll width would not show.
  await expect(rows.first().getByText("High priority")).toBeInViewport({ ratio: 1 });
  await expect(rows.first().locator("time")).toBeInViewport({ ratio: 1 });
  await expectNoHorizontalScroll(page);
  await shot(page, "high-priority");
  await rows.first().getByRole("button").first().tap();
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByText("High priority")).toBeInViewport({ ratio: 1 });
  await expectNoHorizontalScroll(page);
  await dialog.getByRole("button", { name: "Close" }).tap();

  await page.goto("/rules/new");
  await expect(page.getByLabel("Priority")).toBeVisible();
  await page.getByLabel("Priority").selectOption("high");
  await expect(page.getByLabel("Priority")).toHaveValue("high");
  await expectNoHorizontalScroll(page);
});

// 003-AC5: the Talk form, with its long setup commands, fits 375 px and Silent is reachable by touch.
test("the Nextcloud Talk form fits 375 px", async ({ page }) => {
  await page.goto("/channels");
  await page.getByRole("button", { name: "New channel" }).first().tap();
  const dialog = page.getByRole("dialog", { name: "New channel" });
  await dialog.getByText("Nextcloud Talk", { exact: true }).tap();
  const silent = dialog.getByRole("switch", { name: "Silent" });
  await silent.scrollIntoViewIfNeeded();
  await silent.tap();
  await expect(silent).toBeChecked();
  const width = await dialog.evaluate((el) => ({ scroll: el.scrollWidth, client: el.clientWidth }));
  expect(width.scroll).toBeLessThanOrEqual(width.client);
  await expectNoHorizontalScroll(page);
  await shot(page, "talk-form");
});

test.describe("without a session", () => {
  test.use({ storageState: { cookies: [], origins: [] } });

  // 002-AC8: the sign-in gate fits 375 px and its action is reachable by touch.
  test("the sign-in gate fits 375 px", async ({ page }) => {
    for (const [path, heading] of [
      ["/watches/new", "Sign in to continue"],
      ["/inbox?auth=unavailable", "Sign-in service unavailable"],
    ] as const) {
      await page.goto(path);
      await expect(gate(page, heading)).toBeVisible();
      await expect(page.getByRole("link", { name: /^(Sign in|Try again)$/ })).toBeInViewport();
      await expectNoHorizontalScroll(page);
      await shot(page, `gate-${heading.split(" ")[0]?.toLowerCase()}`);
    }
  });
});

// 001-AC26: the sidebar is a drawer, a watch is created, the inbox detail is a dialog.
test("drawer, create watch and inbox dialog at 375 px", async ({ page }) => {
  const t = tag();
  await page.goto("/inbox?state=all");
  const sidebar = page.getByRole("complementary", { name: "Sidebar" });
  await expect(sidebar.getByRole("link", { name: "Watches" })).not.toBeInViewport();
  await page.getByRole("button", { name: "Open navigation" }).tap();
  await expect(sidebar.getByRole("link", { name: "Watches" })).toBeInViewport();
  await sidebar.getByRole("link", { name: "Watches" }).tap();
  await expect(page.getByRole("heading", { level: 1, name: "Watches" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Open navigation" })).toBeVisible();
  await expectNoHorizontalScroll(page);
  await shot(page, "watches");

  await page.goto("/watches/new");
  await page.getByLabel("Paste a URL").fill("https://github.com/cli/cli/releases");
  await page.getByLabel("API URL (optional)").fill(UPSTREAM);
  await page.getByLabel("Name").fill(`Mobile ${t}`);
  await expectNoHorizontalScroll(page);
  await page.getByRole("button", { name: "Create watch" }).tap();
  await expect(page.getByRole("heading", { level: 1, name: `Mobile ${t}` })).toBeVisible();
  await expectNoHorizontalScroll(page);

  await page.goto("/inbox?state=all");
  await page
    .getByRole("list", { name: /^Inbox, / })
    .getByRole("button")
    .first()
    .tap();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toBeVisible();
  await expect(dialog.getByRole("button", { name: "Close" })).toBeFocused();
  const box = await dialog.boundingBox();
  expect(box && box.x >= 0 && box.x + box.width <= 375).toBe(true);
  await expectNoHorizontalScroll(page);
  await shot(page, "inbox-dialog");
  await dialog.getByRole("button", { name: "Close" }).tap();
  await expect(dialog).toBeHidden();
});

// 001-AC26: the rule form and the script editor are usable at 375 px.
test("rule form and script editor at 375 px", async ({ page }) => {
  const t = tag();
  await page.goto("/channels");
  // 003-AC5: a long unbroken name beside a long target keeps the form within 375 px.
  const created = await (
    await api(page)
  )("POST", "/api/v1/channels", {
    name: `Long${t}${"n".repeat(150)}`,
    type: "nextcloud_talk",
    config: {
      url: `https://${"h".repeat(60)}.example.com`,
      conversation: `a${"0".repeat(29)}`,
    },
    secret: "s".repeat(40),
  });
  expect(created.status()).toBe(201);
  await page.reload();
  await expect(page.getByRole("list", { name: "Channels" })).toContainText(`Long${t}`);
  await expectNoHorizontalScroll(page);
  await page.goto("/rules/new");
  await expect(page.getByRole("checkbox", { name: new RegExp(`Long${t}`) })).toBeVisible();
  await expectNoHorizontalScroll(page);
  await page.getByLabel("Name").fill(`Mobile rule ${t}`);
  await page.getByLabel("Watches").selectOption({ label: SEED.watch });
  await enterFilter(page, 'event.type == "release.published"', (text) =>
    page.getByLabel("Expression").fill(text),
  );
  await expectNoHorizontalScroll(page);
  await shot(page, "rule-form");
  await page.getByRole("button", { name: "Create rule" }).tap();
  await expect(page).toHaveURL(/\/rules$/);
  await expect(page.getByRole("list", { name: "Rules" })).toContainText(`Mobile rule ${t}`);

  await page.goto("/scripts/new");
  await page.getByLabel("Name").fill(`Mobile script ${t}`);
  const editor = page.getByRole("textbox", { name: "Source" });
  await editor.tap();
  await page.keyboard.press("ControlOrMeta+a");
  await page.keyboard.press("Delete");
  // A long line must scroll inside the editor, not the page.
  await page.keyboard.insertText(
    `function main(event) {\nlog("a long line that is much wider than a phone screen", event.type, event.summary);\n}\n`,
  );
  await expectNoHorizontalScroll(page);
  await page.getByRole("button", { name: "Run with sample event" }).tap();
  await expect(page.getByTestId("script-results")).toContainText("Returned");
  await expectNoHorizontalScroll(page);
  await shot(page, "script-editor");
  await page.getByRole("button", { name: "Create script" }).tap();
  await expect(
    page.getByRole("heading", { level: 1, name: `Edit Mobile script ${t}` }),
  ).toBeVisible();

  const call = await api(page);
  const scripts = (await (await call("GET", "/api/v1/scripts")).json()).items as { name: string }[];
  expect(scripts.map((s) => s.name)).toContain(`Mobile script ${t}`);
});

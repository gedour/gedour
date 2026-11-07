import AxeBuilder from "@axe-core/playwright";
import type { Locator, Page } from "@playwright/test";

import { BOB_AUTH_FILE } from "./auth-file";
import { addDevice, api, expect, gate, SEED, test } from "./fixtures";

test.use({ storageState: BOB_AUTH_FILE, reducedMotion: "reduce" });

/** The seeded watch and rule ids, for their detail and form views. */
async function seedIds(page: Page) {
  const call = await api(page);
  const watches = (await (await call("GET", "/api/v1/watches")).json()).items as {
    id: string;
    name: string;
  }[];
  const rules = (await (await call("GET", "/api/v1/rules")).json()).items as {
    id: string;
    name: string;
  }[];
  const watch = watches.find((w) => w.name === SEED.watch)?.id;
  const rule = rules.find((r) => r.name === SEED.rule)?.id;
  if (!watch || !rule) throw new Error("seed.setup.ts did not run");
  return { watch, rule };
}

const luminance = (rgb: number[]) => {
  const [r = 0, g = 0, b = 0] = rgb.map((v) => {
    const c = v / 255;
    return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
  });
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
};

/** WCAG contrast of the switch's track against the dialog behind it, and of its knob against the track. */
async function switchContrast(silent: Locator) {
  const colors = await silent.evaluate((el) => {
    const knob = el.querySelector("span") as Element;
    const dialog = el.closest("dialog") as Element;
    return [el, knob, dialog].map((e) => getComputedStyle(e).backgroundColor);
  });
  const [track, knob, surface] = colors.map((c) =>
    luminance((c.match(/[\d.]+/g) ?? []).map(Number)),
  ) as [number, number, number];
  const ratio = (a: number, b: number) => (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
  return Math.min(ratio(track, surface), ratio(knob, track));
}

/** Serious and critical axe violations on the page as it is now, one line per rule and node. */
async function violations(page: Page): Promise<string[]> {
  const { violations } = await new AxeBuilder({ page }).analyze();
  return violations
    .filter((v) => v.impact === "serious" || v.impact === "critical")
    .flatMap((v) => v.nodes.map((n) => `${v.id}: ${n.target.join(" ")} ${n.failureSummary}`));
}

// 001-AC26: no serious or critical axe violation on any main view, in either theme.
for (const theme of ["light", "dark"] as const) {
  test.describe(`${theme} theme`, () => {
    test.beforeEach(async ({ page }) => {
      await page.addInitScript((t) => localStorage.setItem("gedour.theme", t), theme);
    });

    test("main views have no serious axe violation", async ({ page }) => {
      await page.goto("/inbox");
      const { watch, rule } = await seedIds(page);
      const views: [string, string][] = [
        ["/inbox?state=all", "Inbox"],
        // 005-AC7: the filter pressed and the badge, the high rule's badge in /rules.
        ["/inbox?priority=high", "Inbox"],
        ["/watches", "Watches"],
        [`/watches/${watch}`, SEED.watch],
        ["/watches/new", "New watch"],
        ["/rules", "Rules"],
        ["/rules/new", "New rule"],
        [`/rules/${rule}`, `Edit ${SEED.rule}`],
        ["/channels", "Channels"],
        ["/scripts", "Scripts"],
        ["/scripts/new", "New script"],
        ["/audit", "Audit"],
        ["/settings", "Settings"],
        ["/settings/credentials", "Credentials"],
        ["/keys", "API keys"],
      ];
      const found: Record<string, string[]> = {};
      for (const [path, heading] of views) {
        await page.goto(path);
        await expect(page.getByRole("heading", { level: 1, name: heading })).toBeVisible();
        await expect(page.locator("[aria-busy=true]")).toHaveCount(0);
        // 008-AC15: the default kind's form, its new-items boxes and their hint, is the one checked.
        if (path === "/watches/new") {
          await expect(
            page.getByRole("checkbox", { name: "Report new pull requests" }),
          ).toBeVisible();
          await expect(page.getByText(/1 more request:/)).toBeVisible();
          // 011-AC17: and the discussions box with its own hint.
          await expect(page.getByText(/2 to 3 more requests/)).toBeVisible();
        }
        found[path] = await violations(page);
        // 011-AC17: the github_discussion form too.
        if (path === "/watches/new") {
          await page
            .getByRole("radio", { name: /GitHub discussion/ })
            .locator("..")
            .click();
          await expect(page.getByLabel("Discussion number")).toBeVisible();
          found[`${path} github_discussion`] = await violations(page);
        }
      }
      expect(found).toEqual({
        ...Object.fromEntries(views.map(([p]) => [p, []])),
        "/watches/new github_discussion": [],
      });
    });

    test("dialogs and the inbox panel have no serious axe violation", async ({ page }) => {
      await page.goto("/inbox?state=all");
      await page
        .getByRole("list", { name: /^Inbox, / })
        .getByRole("button")
        .first()
        .click();
      await expect(page.getByRole("button", { name: "Close details" })).toBeVisible();
      expect(await violations(page)).toEqual([]);
      // 004-AC7: the release notes, open.
      await page.getByText("Release notes").first().click();
      await expect(page.locator("details[open] pre").first()).toBeVisible();
      expect(await violations(page)).toEqual([]);

      const device = await addDevice(page, `a11y ${theme}`);
      await page.goto("/channels");
      await expect(page.getByRole("heading", { level: 1, name: "Channels" })).toBeVisible();
      expect(await violations(page)).toEqual([]);
      await page.getByRole("button", { name: "New channel" }).first().click();
      const dialog = page.getByRole("dialog", { name: "New channel" });
      await expect(dialog).toBeVisible();
      expect(await violations(page)).toEqual([]);

      // 003-AC5: the forms with the Silent switch, off and on.
      for (const kind of ["Telegram", "Nextcloud Talk"]) {
        await dialog.getByText(kind, { exact: true }).click();
        const silent = dialog.getByRole("switch", { name: "Silent" });
        expect(await violations(page)).toEqual([]);
        // WCAG 1.4.11; the poll waits out the colour transition.
        await expect.poll(() => switchContrast(silent)).toBeGreaterThanOrEqual(3);
        await silent.click();
        await expect(silent).toBeChecked();
        expect(await violations(page)).toEqual([]);
        await expect.poll(() => switchContrast(silent)).toBeGreaterThanOrEqual(3);
        await silent.click();
      }

      // 010-AC15, 010-AC18: the Web Push form with its device list, all then picked, and its Silent switch.
      await dialog.getByText("Web Push", { exact: true }).click();
      const devices = dialog.getByRole("list", { name: "My devices" });
      await expect(devices).toContainText(device);
      expect(await violations(page)).toEqual([]);
      await dialog.getByLabel(/Pick devices/).check();
      await expect(devices.getByRole("checkbox", { name: device })).toBeVisible();
      expect(await violations(page)).toEqual([]);
      const pushSilent = dialog.getByRole("switch", { name: "Silent" });
      await pushSilent.click();
      await expect(pushSilent).toBeChecked();
      await expect.poll(() => switchContrast(pushSilent)).toBeGreaterThanOrEqual(3);
      expect(await violations(page)).toEqual([]);
      // 010-AC15: the device rename and remove dialogs, over the channel form.
      await devices.getByRole("button", { name: `Rename ${device}` }).click();
      const rename = page.getByRole("dialog", { name: `Rename ${device}` });
      await expect(rename).toBeVisible();
      expect(await violations(page)).toEqual([]);
      await page.keyboard.press("Escape");
      await expect(rename).toBeHidden();
      await devices.getByRole("button", { name: `Remove ${device}` }).click();
      const remove = page.getByRole("dialog", { name: `Remove ${device}?` });
      await expect(remove).toBeVisible();
      expect(await violations(page)).toEqual([]);
      await page.keyboard.press("Escape");
      await expect(remove).toBeHidden();
      await expect(dialog).toBeVisible();
      await page.keyboard.press("Escape");
      await expect(dialog).toBeHidden();

      // 007-AC32: the create form with the manage warning, the reveal and the revoke confirmation.
      const name = `a11y ${theme} ${Date.now().toString(36)}`;
      await page.goto("/keys");
      await page.getByRole("button", { name: "New API key" }).click();
      const create = page.getByRole("dialog", { name: "New API key" });
      await create.getByLabel("Name", { exact: true }).fill(name);
      await create.getByRole("radio", { name: "Manage" }).check();
      await expect(create.getByText("silence alerts")).toBeVisible();
      expect(await violations(page)).toEqual([]);
      await create.getByRole("button", { name: "Create key" }).click();
      await expect(page.getByTestId("api-key-reveal")).toBeVisible();
      expect(await violations(page)).toEqual([]);
      await page.getByRole("button", { name: "I have copied it" }).click();
      await page
        .getByRole("row")
        .filter({ hasText: name })
        .getByRole("button", { name: "Revoke" })
        .click();
      const revoke = page.getByRole("dialog", { name: `Revoke ${name}?` });
      await expect(revoke).toBeVisible();
      expect(await violations(page)).toEqual([]);
      await revoke.getByRole("button", { name: "Revoke" }).click();
      await expect(page.getByRole("row").filter({ hasText: name })).toContainText("revoked");
      expect(await violations(page)).toEqual([]);
    });

    // 005-AC7: the badge in the panel, the form's control on a high rule.
    test("the high priority panel and rule form have no serious axe violation", async ({
      page,
    }) => {
      await page.goto("/inbox?priority=high");
      await page
        .getByRole("list", { name: /^Inbox, / })
        .getByRole("button")
        .first()
        .click();
      await expect(page.getByRole("complementary").getByText("High priority")).toBeVisible();
      expect(await violations(page)).toEqual([]);

      const rules = (await (await (await api(page))("GET", "/api/v1/rules")).json()).items as {
        id: string;
        name: string;
      }[];
      await page.goto(`/rules/${rules.find((r) => r.name === SEED.highRule)?.id}`);
      await expect(page.getByLabel("Priority")).toHaveValue("high");
      expect(await violations(page)).toEqual([]);
    });

    test("the signed-out page has no serious axe violation", async ({ page }) => {
      await page.goto("/auth/signed-out");
      await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
      expect(await violations(page)).toEqual([]);
    });

    test.describe("without a session", () => {
      test.use({ storageState: { cookies: [], origins: [] } });

      // 002-AC8: the sign-in gate, with and without a reason.
      test("the sign-in gate has no serious axe violation", async ({ page }) => {
        const found: Record<string, string[]> = {};
        for (const [path, heading] of [
          ["/inbox", "Sign in to continue"],
          ["/inbox?auth=unavailable", "Sign-in service unavailable"],
          ["/inbox?auth=failed", "Sign-in failed, try again"],
        ] as const) {
          await page.goto(path);
          await expect(gate(page, heading)).toBeVisible();
          found[path] = await violations(page);
        }
        expect(found).toEqual({
          "/inbox": [],
          "/inbox?auth=unavailable": [],
          "/inbox?auth=failed": [],
        });
      });
    });
  });
}

import type { Page } from "@playwright/test";

import { ERIN_AUTH_FILE } from "./auth-file";
import { api, expect, test, UPSTREAM } from "./fixtures";

// Erin's own account: 2000 watches in bob's or alice's lists would push their seeded ones past a page.
test.use({ storageState: ERIN_AUTH_FILE });

const SHOTS = process.env.E2E_SHOTS_DIR;
const FLEET = 2000;
const pad = (i: number) => String(i).padStart(4, "0");

/** Fleet 0000…1999, paused (never polled): even ones GitHub fleet/repo-i, odd ones OCI fleet/img-i. */
function fleetWatch(i: number) {
  const base = { name: `Fleet ${pad(i)}`, paused: true };
  return i % 2
    ? {
        ...base,
        kind: "oci_tags",
        config: { registry: UPSTREAM.replace("http://", ""), repository: `fleet/img-${pad(i)}` },
      }
    : {
        ...base,
        kind: "github_repo",
        config: { repo: `fleet/repo-${pad(i)}`, api_url: UPSTREAM },
      };
}

/** Creates the fleet watches erin lacks: a retry or a reused database keeps what is there. */
async function seedFleet(page: Page) {
  const call = await api(page);
  const have = new Set<string>();
  // 100 names per prefix, under the 500 a page holds.
  for (let h = 0; h < FLEET / 100; h++) {
    const res = await call(
      "GET",
      `/api/v1/watches?q=${encodeURIComponent(`Fleet ${pad(h * 100).slice(0, 2)}`)}`,
    );
    for (const w of (await res.json()).items as { name: string }[]) have.add(w.name);
  }
  const missing = Array.from({ length: FLEET }, (_, i) => i).filter(
    (i) => !have.has(`Fleet ${pad(i)}`),
  );
  for (let at = 0; at < missing.length; at += 50) {
    const statuses = await Promise.all(
      missing
        .slice(at, at + 50)
        .map(async (i) => (await call("POST", "/api/v1/watches", fleetWatch(i))).status()),
    );
    expect(statuses.filter((s) => s !== 201)).toEqual([]);
  }
}

const rows = (page: Page) =>
  page.getByRole("list", { name: "Watches to pick" }).getByRole("checkbox");
const search = (page: Page) => page.getByRole("searchbox", { name: "Search" });

/** Runs act, then waits for the list it asks for (debounced): rows from the previous search stay until then. */
async function listed(page: Page, act: () => Promise<unknown>, param: "q" | "kind", value: string) {
  const res = page.waitForResponse((r) => {
    const u = new URL(r.url());
    return (
      u.pathname === "/api/v1/watches" &&
      u.searchParams.get("limit") === "101" &&
      (u.searchParams.get(param) ?? "") === value
    );
  });
  await act();
  await res;
}
const searchFor = (page: Page, q: string) =>
  listed(page, () => search(page).fill(q), "q", q.trim());
const kindTo = (page: Page, label: string, kind: string) =>
  listed(
    page,
    () => page.getByRole("combobox", { name: "Kind" }).selectOption({ label }),
    "kind",
    kind,
  );
const shot = async (page: Page, name: string) => {
  if (!SHOTS) return;
  // The default theme follows the system: the emulated scheme switches it.
  for (const colorScheme of ["light", "dark"] as const) {
    await page.emulateMedia({ colorScheme });
    await page
      .getByRole("group", { name: "Watches" })
      .screenshot({ path: `${SHOTS}/014-${name}-${colorScheme}.png` });
  }
  await page.emulateMedia({ colorScheme: "light" });
};

// The screenshots need the fleet the first test creates.
test.describe.configure({ mode: "serial", timeout: 120_000 });

// 014-AC15, 014-AC16: the picker on 2000 watches: the first 100 and a note, search on name and target
// with the term marked, the kind narrowing further, no match; select all matching past the 100 shown,
// the 500 refusal, clear matching, selected only, the count; the saved rule lists them all.
test("pick watches among 2000", async ({ page }) => {
  await page.goto("/rules/new");
  await expect(page.getByRole("heading", { level: 1, name: "New rule" })).toBeVisible();
  await seedFleet(page);
  const name = `Fleet rule ${Date.now().toString(36)}`;
  await page.getByLabel("Name").fill(name);
  await page.getByRole("radio", { name: /^Selected watches/ }).check();

  // 014-AC15
  await expect(rows(page)).toHaveCount(100);
  await expect(page.getByText("Showing the first 100 matches: refine the search")).toBeVisible();
  await expect(rows(page).first()).toHaveAccessibleName(/^Fleet 0000 fleet\/repo-0000 releases/);
  await searchFor(page, "repo-19");
  // Name or target: repo-19xx are the even ones of 1900…1999.
  await expect(rows(page)).toHaveCount(50);
  await expect(page.getByText(/Showing the first 100/)).toBeHidden();
  await expect(rows(page).first().locator("xpath=..").locator("mark")).toHaveText("repo-19");
  await searchFor(page, "fleet 1");
  await expect(rows(page)).toHaveCount(100);
  await expect(page.getByText("Showing the first 100 matches: refine the search")).toBeVisible();
  await kindTo(page, "Image tags", "oci_tags");
  await expect(rows(page).first()).toHaveAccessibleName(/^Fleet 1001 /);
  await searchFor(page, "fleet 19");
  await expect(rows(page)).toHaveCount(50);
  await searchFor(page, "no such fleet");
  await expect(page.getByText("No watch matches the search.")).toBeVisible();
  await kindTo(page, "All kinds", "");

  // 014-AC16: Fleet 1xxx is 1000 matches: refused, nothing picked.
  await searchFor(page, "fleet 1");
  await expect(rows(page)).toHaveCount(100);
  await page.getByRole("button", { name: "Select all matching" }).click();
  const picker = page.getByRole("group", { name: "Watches" });
  await expect(picker.getByRole("status")).toHaveText(
    "500 or more watches match: refine the search. Nothing was picked.",
  );
  await expect(page.getByText("0 selected", { exact: true })).toBeVisible();
  // One pick, then Fleet 10xx and 11xx: 200 matches, past the 100 shown, the pick kept.
  await searchFor(page, "fleet 0005");
  await rows(page).first().check();
  const each = async (qs: string[], button: string) => {
    for (const q of qs) {
      await searchFor(page, q);
      await expect(rows(page).first()).toHaveAccessibleName(new RegExp(`^Fleet ${q.slice(-2)}00 `));
      await page.getByRole("button", { name: button }).click();
    }
  };
  await each(["fleet 10", "fleet 11"], "Select all matching");
  await expect(page.getByText("201 selected", { exact: true })).toBeVisible();
  await each(["fleet 12", "fleet 13"], "Select all matching");
  await expect(page.getByText("401 selected", { exact: true })).toBeVisible();
  // 100 matches, but 501 picks in all: refused, the count unchanged.
  await each(["fleet 14"], "Select all matching");
  await expect(picker.getByRole("status")).toHaveText(
    "That would pick 501 watches, and a rule takes 500 at most. Nothing was picked.",
  );
  await expect(page.getByText("401 selected", { exact: true })).toBeVisible();
  // Clear matching takes the matches only: Fleet 0005 and 10xx stay.
  await each(["fleet 11", "fleet 12", "fleet 13"], "Clear matching");
  await expect(page.getByText("101 selected", { exact: true })).toBeVisible();
  await page.getByRole("checkbox", { name: "Selected only" }).check();
  await expect(rows(page)).toHaveCount(101);
  await expect(rows(page).first()).toHaveAccessibleName(/^Fleet 0005 /);
  await page.getByRole("checkbox", { name: "Selected only" }).uncheck();

  await page.getByRole("button", { name: "Create rule" }).click();
  await expect(page).toHaveURL(/\/rules$/);
  const call = await api(page);
  const rules = (await (await call("GET", "/api/v1/rules")).json()).items as {
    id: string;
    name: string;
    watches: { name: string }[];
  }[];
  const saved = rules.find((r) => r.name === name);
  try {
    expect(saved?.watches.map((w) => w.name).sort()).toEqual([
      "Fleet 0005",
      ...Array.from({ length: 100 }, (_, i) => `Fleet ${pad(1000 + i)}`),
    ]);
    // 014-AC19: the first two names in order, then +99 opening the 101.
    const row = page.getByRole("listitem").filter({ hasText: name });
    await expect(row.getByText("Fleet 0005, Fleet 1000", { exact: true })).toBeVisible();
    await expect(row.getByRole("button", { name: "Show all 101 watches" })).toHaveText("+99");
  } finally {
    // Rules are capped per user: a retry must not pile them up.
    for (const r of rules.filter((r) => r.name.startsWith("Fleet rule ")))
      expect((await call("DELETE", `/api/v1/rules/${r.id}`)).ok()).toBe(true);
  }
});

// Screenshots for the merge (E2E_SHOTS_DIR): the picker empty, searching the fleet, 30 picked.
test("picker screenshots", async ({ page }) => {
  test.skip(!SHOTS, "E2E_SHOTS_DIR unset");
  await page.goto("/rules/new");
  await seedFleet(page);
  await page.setViewportSize({ width: 1100, height: 1000 });
  await page.route("**/api/v1/watches?*", (r) => r.fulfill({ json: { items: [] } }));
  await page.goto("/rules/new");
  await page.getByRole("radio", { name: /^Selected watches/ }).check();
  await expect(page.getByText(/You have no watch yet/)).toBeVisible();
  await shot(page, "picker-empty");
  await page.unroute("**/api/v1/watches?*");
  await page.reload();
  await page.getByRole("radio", { name: /^Selected watches/ }).check();
  await searchFor(page, "repo-1");
  await expect(page.getByText("Showing the first 100 matches: refine the search")).toBeVisible();
  await shot(page, "picker-search");
  await searchFor(page, "fleet 00");
  await expect(rows(page).first()).toHaveAccessibleName(/^Fleet 0000 /);
  for (let i = 0; i < 30; i++) await rows(page).nth(i).check();
  await searchFor(page, "");
  await expect(page.getByText("30 selected", { exact: true })).toBeVisible();
  await shot(page, "picker-30-picked");
});

import type { Browser, Page, Response } from "@playwright/test";

import { BOB_AUTH_FILE } from "./auth-file";
import { ALICE, api, expect, test, UPSTREAM, watchCsp } from "./fixtures";

// Each run gets its own names: specs run in parallel against one database.
const tag = () => `${Date.now().toString(36)}${Math.floor(Math.random() * 1e4)}`;

async function createWatch(page: Page, name: string) {
  const call = await api(page);
  const res = await call("POST", "/api/v1/watches", {
    name,
    kind: "github_repo",
    config: { repo: "golang/go", api_url: UPSTREAM },
  });
  expect(res.status()).toBe(201);
  return (await res.json()).id as string;
}

/** Bob's page, with the same CSP guard the fixture gives alice's. */
async function bobPage(browser: Browser, baseURL: string | undefined) {
  const context = await browser.newContext({ storageState: BOB_AUTH_FILE });
  const page = await context.newPage();
  const violations = await watchCsp(page, baseURL);
  return { page, violations, close: () => context.close() };
}

test("alice deletes several watches at once", async ({ page }) => {
  const t = tag();
  for (const n of ["one", "two", "three"]) await createWatch(page, `Bulk ${n} ${t}`);
  await page.goto("/watches");
  await page.getByLabel("Search").fill(t);
  const list = page.getByRole("list", { name: "Watches" });
  await expect(list.getByRole("listitem")).toHaveCount(3);
  // Rows below the fold: scrolled to the bottom, the header and the sidebar still frame the page.
  await page.setViewportSize({ width: 1280, height: 320 });
  const shellHolds = () =>
    page.evaluate(() => {
      window.scrollTo(0, document.documentElement.scrollHeight);
      const header = document.querySelector("header")?.getBoundingClientRect();
      const sidebar = document.getElementById("app-sidebar")?.getBoundingClientRect();
      return window.scrollY > 0 && header?.top === 0 && sidebar?.bottom === window.innerHeight;
    });
  expect(await shellHolds()).toBe(true);
  await page.getByRole("button", { name: `Select: Bulk one ${t}` }).click();
  await page.getByRole("button", { name: `Select: Bulk two ${t}` }).click();
  await expect(page.getByRole("button", { name: `Select: Bulk two ${t}` })).toHaveAttribute(
    "aria-pressed",
    "true",
  );
  await page
    .getByRole("toolbar", { name: "Bulk actions" })
    .getByRole("button", { name: "Delete" })
    .click();
  await page
    .getByRole("dialog", { name: "Delete 2 watches?" })
    .getByRole("button", { name: "Delete" })
    .click();
  await expect(page.getByRole("status")).toContainText("Deleted 2 watches");
  await expect(list.getByRole("listitem")).toHaveCount(1);
  await expect(list).toContainText(`Bulk three ${t}`);
  expect(await shellHolds()).toBe(true);
});

// 001-AC4: one watch of each kind through the UI (one by URL paste), edit, pause, resume, delete.
test("alice creates, edits, pauses, resumes and deletes watches of every kind", async ({
  page,
}) => {
  const t = tag();
  await page.goto("/watches/new");
  await page.getByLabel("Paste a URL").fill("https://github.com/golang/go/releases");
  await expect(page.getByText("Recognized: GitHub repository")).toBeVisible();
  await expect(page.getByRole("radio", { name: /GitHub repository/ })).toBeChecked();
  await expect(page.getByLabel("Repository", { exact: true })).toHaveValue("golang/go");
  await page.getByLabel("API URL (optional)").fill(UPSTREAM);
  await page.getByLabel("Name").fill(`Go releases ${t}`);
  await page.getByRole("button", { name: "Create watch" }).click();
  await expect(page.getByRole("heading", { level: 1, name: `Go releases ${t}` })).toBeVisible();

  const manual: { kind: RegExp; name: string; fields: [string, string][] }[] = [
    {
      kind: /GitHub pull request/,
      name: `Go PR ${t}`,
      fields: [
        ["Repository", "golang/go"],
        ["Pull request number", "1"],
        ["API URL (optional)", UPSTREAM],
      ],
    },
    {
      kind: /GitLab releases/,
      name: `GitLab ${t}`,
      fields: [
        ["Project path", "gitlab-org/gitlab"],
        ["GitLab URL (optional)", UPSTREAM],
      ],
    },
    {
      kind: /GitLab merge request/,
      name: `GitLab MR ${t}`,
      fields: [
        ["Project path", "gitlab-org/gitlab"],
        ["Merge request number", "2"],
        ["GitLab URL (optional)", UPSTREAM],
      ],
    },
    {
      kind: /Image tags/,
      name: `postgres ${t}`,
      fields: [
        ["Registry (optional)", UPSTREAM.replace("http://", "")],
        ["Repository", "library/postgres"],
        ["Tag filter (optional)", "^\\d+$"],
      ],
    },
  ];
  for (const w of manual) {
    await page.goto("/watches/new");
    // The radio is visually hidden inside its card: click the card, like a user does.
    await page.getByRole("radio", { name: w.kind }).locator("..").click();
    await expect(page.getByRole("radio", { name: w.kind })).toBeChecked();
    await page.getByLabel("Name").fill(w.name);
    for (const [label, value] of w.fields)
      await page.getByLabel(label, { exact: true }).fill(value);
    await page.getByRole("button", { name: "Create watch" }).click();
    await expect(page.getByRole("heading", { level: 1, name: w.name })).toBeVisible();
  }
  await expect(page.getByRole("region", { name: "Configuration" })).toContainText(
    "library/postgres",
  );

  await page.goto("/watches");
  await page.getByLabel("Search").fill(t);
  await expect(page.getByRole("list", { name: "Watches" }).getByRole("listitem")).toHaveCount(5);

  // Field-level error from the server's 400.
  await page.getByRole("link", { name: new RegExp(`Go PR ${t}`) }).click();
  await page.getByRole("link", { name: "Edit" }).click();
  await page.getByLabel("Schedule", { exact: true }).fill("61 * * * *");
  await page.getByRole("button", { name: "Save changes" }).click();
  await expect(page.getByLabel("Schedule", { exact: true })).toHaveAttribute(
    "aria-invalid",
    "true",
  );
  await expect(page.getByText(/invalid cron expression/)).toBeVisible();
  await page.getByRole("button", { name: "Daily 09:00" }).click();
  await page.getByLabel("Name").fill(`Go PR renamed ${t}`);
  await page.getByRole("button", { name: "Save changes" }).click();
  await expect(page.getByRole("heading", { level: 1, name: `Go PR renamed ${t}` })).toBeVisible();
  await expect(page.getByRole("region", { name: "Configuration" })).toContainText(
    "Every day at 09:00",
  );

  const paused = page.getByRole("switch", { name: "Paused" });
  await paused.click();
  await expect(paused).toHaveAttribute("aria-checked", "true");
  await paused.click();
  await expect(paused).toHaveAttribute("aria-checked", "false");

  await page.getByRole("button", { name: "Delete watch" }).click();
  const dialog = page.getByRole("dialog", { name: `Delete Go PR renamed ${t}?` });
  await expect(dialog).toBeVisible();
  await dialog.getByRole("button", { name: "Delete", exact: true }).click();
  await expect(page.getByRole("heading", { level: 1, name: "Watches" })).toBeVisible();
  await page.getByLabel("Search").fill(t);
  await expect(page.getByRole("list", { name: "Watches" }).getByRole("listitem")).toHaveCount(4);

  const audit = await (await page.request.get("/api/v1/audit?target_type=watch&limit=200")).json();
  const actions = audit.items.map((i: { action: string }) => i.action);
  for (const a of [
    "watch.created",
    "watch.updated",
    "watch.paused",
    "watch.resumed",
    "watch.deleted",
  ]) {
    expect(actions).toContain(a);
  }
});

// 009-AC6: bob opening alice's watch sees the not-found view; every watch route answers 404, never 403.
test("another user's watch reads as not found", async ({ page, browser, baseURL }) => {
  const t = tag();
  const id = await createWatch(page, `Private ${t}`);
  const bob = await bobPage(browser, baseURL);
  const bobApi = await api(bob.page);

  await bob.page.goto(`/watches/${id}`);
  await expect(bob.page.getByRole("heading", { name: "Page not found" })).toBeVisible();
  const path = `/api/v1/watches/${id}`;
  const calls: [string, string, unknown?][] = [
    ["GET", path],
    ["PATCH", path, { name: "x" }],
    ["DELETE", path],
    ["POST", `${path}/run`],
    ["POST", `${path}/pause`],
    ["POST", `${path}/resume`],
    ["GET", `${path}/states`],
    ["GET", `${path}/events`],
  ];
  for (const [method, url, data] of calls) {
    expect((await bobApi(method, url, data)).status(), `${method} ${url}`).toBe(404);
  }
  const listed = await (await bobApi("GET", `/api/v1/watches?q=${t}`)).json();
  expect(listed.items).toEqual([]);
  expect((await (await api(page))("GET", path)).status()).toBe(200);

  expect(bob.violations).toEqual([]);
  await bob.close();
});

// 001-AC6: the secret goes in once and never comes back, in the UI or in any API response.
test("credentials never return their secret", async ({ page }) => {
  const t = tag();
  const secret = `ghp_e2e${t}SECRETvalue`;
  const bodies: Promise<string | null>[] = [];
  // A read still open when page.goto navigates away may never settle: that body is lost, not a hang.
  const read = (res: Response) => {
    let timer: NodeJS.Timeout | undefined;
    const lost = new Promise<null>((resolve) => {
      timer = setTimeout(() => resolve(null), 5_000);
    });
    return Promise.race([res.text().catch(() => null), lost]).finally(() => clearTimeout(timer));
  };
  page.on("response", (res) => {
    const path = new URL(res.url()).pathname;
    // The inbox stream never ends: its body would never resolve.
    if (path.startsWith("/api/") && path !== "/api/v1/inbox/stream") bodies.push(read(res));
  });
  await page.goto("/settings/credentials");
  await page.getByRole("button", { name: "Add credential" }).click();
  const dialog = page.getByRole("dialog", { name: "Add a credential" });
  await dialog.getByLabel("Name").fill(`Bot ${t}`);
  await dialog.getByLabel("Token").fill(secret);
  await dialog.getByRole("button", { name: "Save credential" }).click();
  const row = page.getByRole("row").filter({ hasText: `Bot ${t}` });
  await expect(row).toContainText(`••••${secret.slice(-4)}`);
  await expect(row).toContainText("0 watches");

  // Attach it to a watch: the 409 on delete names the watch.
  await page.goto("/watches/new");
  await page.getByLabel("Paste a URL").fill("https://github.com/golang/go");
  await page.getByLabel("API URL (optional)").fill(UPSTREAM);
  await page.getByLabel("Name").fill(`With cred ${t}`);
  await page
    .getByLabel("Credential (optional)")
    .selectOption({ label: `Bot ${t} (••••${secret.slice(-4)})` });
  await page.getByRole("button", { name: "Create watch" }).click();
  await expect(page.getByRole("region", { name: "Configuration" })).toContainText(`Bot ${t}`);
  await page.goto("/settings/credentials");
  await page
    .getByRole("row")
    .filter({ hasText: `Bot ${t}` })
    .getByRole("button", { name: "Delete" })
    .click();
  await page.getByRole("dialog").getByRole("button", { name: "Delete", exact: true }).click();
  await expect(page.getByRole("dialog").getByRole("alert")).toContainText(`With cred ${t}`);

  expect(await page.content()).not.toContain(secret);
  bodies.push(page.request.get("/api/v1/credentials").then((r) => r.text()));
  bodies.push(page.request.get("/api/v1/audit?target_type=credential").then((r) => r.text()));
  const all = (await Promise.all(bodies)).filter((b) => b !== null);
  expect(all.length).toBeGreaterThan(3);
  for (const b of all) expect(b).not.toContain(secret);
});

// 001-AC22: the actor filter lists who acted and narrows the view with the target type.
test("audit filters by actor and target type", async ({ page }) => {
  const t = tag();
  const id = await createWatch(page, `Audited ${t}`);

  await page.goto("/audit");
  await expect(page.getByLabel("Actor").getByRole("option", { name: ALICE.name })).toHaveCount(1);
  await page.getByLabel("Actor").selectOption({ label: ALICE.name });
  await page.getByLabel("Target type").selectOption("watch");
  const table = page.getByRole("table", { name: "Audit entries, newest first" });
  const created = table
    .getByRole("row")
    .filter({ hasText: "watch.created" })
    .filter({ hasText: id.slice(0, 8) });
  await expect(created).toHaveCount(1);
  await expect(created).toContainText(ALICE.name);
});

// 001-AC24: each list view has distinct empty, loading and error states.
for (const view of [
  {
    path: "/watches",
    api: "/api/v1/watches",
    empty: "No watches yet",
    loading: "watch-skeleton",
    error: "Watches could not be loaded.",
  },
  {
    path: "/settings/credentials",
    api: "/api/v1/credentials",
    empty: "No credentials yet",
    loading: "credential-skeleton",
    error: "Credentials could not be loaded.",
  },
  {
    path: "/keys",
    api: "/api/v1/api-keys",
    empty: "No API keys yet",
    loading: "api-key-skeleton",
    error: "API keys could not be loaded.",
  },
  {
    path: "/audit",
    api: "/api/v1/audit",
    empty: "No activity recorded",
    loading: "audit-skeleton",
    error: "The audit log could not be loaded.",
  },
]) {
  test(`${view.path} has empty, loading and error states`, async ({ page }) => {
    const list = (url: URL) => url.pathname === view.api;
    await page.route(list, (r) => r.fulfill({ json: { items: [] } }));
    await page.goto(view.path);
    await expect(page.getByRole("heading", { name: view.empty })).toBeVisible();

    let release = () => {};
    const held = new Promise<void>((r) => (release = r));
    await page.unroute(list);
    await page.route(list, async (r) => {
      await held;
      await r.fulfill({
        status: 500,
        contentType: "application/problem+json",
        json: { status: 500, detail: "boom" },
      });
    });
    await page.reload();
    await expect(page.getByTestId(view.loading).first()).toBeVisible();
    release();
    // The app's query client retries 3 times with backoff before it gives up.
    await expect(page.getByRole("alert")).toContainText(view.error, { timeout: 15_000 });
    await expect(page.getByRole("button", { name: "Retry" })).toBeVisible();
  });
}

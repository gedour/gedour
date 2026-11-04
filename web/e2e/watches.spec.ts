import type { Browser, Page } from "@playwright/test";

import { BOB_AUTH_FILE } from "./auth-file";
import { ALICE, expect, test, watchCsp } from "./fixtures";

// Watches target the fake upstream: e2e never polls the real forges (scripts/e2e.sh checks the log).
const UPSTREAM = process.env.E2E_UPSTREAM_URL ?? "http://127.0.0.1:18091";

// Each run gets its own names: specs run in parallel against one database.
const tag = () => `${Date.now().toString(36)}${Math.floor(Math.random() * 1e4)}`;

/** Calls the API as the page's user, with the session's CSRF token. */
async function api(page: Page) {
  const { csrf_token } = await (await page.request.get("/api/v1/me")).json();
  return (method: string, path: string, data?: unknown) =>
    page.request.fetch(path, { method, data, headers: { "X-CSRF-Token": csrf_token } });
}

async function createWatch(page: Page, name: string) {
  const call = await api(page);
  const res = await call("POST", "/api/v1/watches", {
    name,
    kind: "github_release",
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

// 001-AC4: one watch of each kind through the UI (one by URL paste), edit, pause, resume, delete.
test("alice creates, edits, pauses, resumes and deletes watches of every kind", async ({
  page,
}) => {
  const t = tag();
  await page.goto("/watches/new");
  await page.getByLabel("Paste a URL").fill("https://github.com/golang/go/releases");
  await expect(page.getByText("Recognized: GitHub releases")).toBeVisible();
  await expect(page.getByRole("radio", { name: /GitHub releases/ })).toBeChecked();
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

// 001-AC5: bob sees nothing until shared; ro hides edit controls and the API refuses writes; rw edits
// but cannot delete; a non-shared watch reads as not found.
test("sharing with bob, read-only then read-write", async ({ page, browser, baseURL }) => {
  const t = tag();
  const id = await createWatch(page, `Shared ${t}`);
  const hidden = await createWatch(page, `Private ${t}`);
  const bob = await bobPage(browser, baseURL);
  const bobApi = await api(bob.page);

  await bob.page.goto(`/watches/${id}`);
  await expect(bob.page.getByRole("heading", { name: "Page not found" })).toBeVisible();
  expect((await bobApi("PATCH", `/api/v1/watches/${id}`, { name: "x" })).status()).toBe(404);

  await page.goto(`/watches/${id}`);
  await page.getByLabel("Email").fill("bob@example.test");
  await page.getByRole("button", { name: "Share" }).click();
  await expect(page.getByRole("list", { name: "Shared with" })).toContainText("bob@example.test");

  await bob.page.reload();
  await expect(bob.page.getByRole("heading", { level: 1, name: `Shared ${t}` })).toBeVisible();
  await expect(bob.page.getByText(`Shared by ${ALICE.name} (ro)`)).toBeVisible();
  await expect(bob.page.getByRole("link", { name: "Edit" })).toHaveCount(0);
  await expect(bob.page.getByRole("switch", { name: "Paused" })).toHaveCount(0);
  await expect(bob.page.getByRole("button", { name: "Delete watch" })).toHaveCount(0);
  expect((await bobApi("PATCH", `/api/v1/watches/${id}`, { name: "x" })).status()).toBe(403);

  await page.getByLabel("Access of bob@example.test").selectOption("rw");
  await expect
    .poll(async () => (await (await bobApi("GET", `/api/v1/watches/${id}`)).json()).permission)
    .toBe("rw");
  await bob.page.reload();
  await bob.page.getByRole("link", { name: "Edit" }).click();
  await expect(bob.page.getByLabel("Credential (optional)")).toHaveCount(0);
  await bob.page.getByLabel("Name").fill(`Bob renamed ${t}`);
  await bob.page.getByRole("button", { name: "Save changes" }).click();
  await expect(bob.page.getByRole("heading", { level: 1, name: `Bob renamed ${t}` })).toBeVisible();
  await expect(bob.page.getByRole("button", { name: "Delete watch" })).toHaveCount(0);
  expect((await bobApi("DELETE", `/api/v1/watches/${id}`)).status()).toBe(403);

  await bob.page.goto(`/watches/${hidden}`);
  await expect(bob.page.getByRole("heading", { name: "Page not found" })).toBeVisible();
  await bob.page.goto("/watches");
  await bob.page.getByRole("button", { name: "Shared with me" }).click();
  await expect(bob.page.getByRole("list", { name: "Watches" })).toContainText(`Bob renamed ${t}`);
  await expect(bob.page.getByRole("list", { name: "Watches" })).not.toContainText(`Private ${t}`);

  expect(bob.violations).toEqual([]);
  await bob.close();
});

// 001-AC6: the secret goes in once and never comes back, in the UI or in any API response.
test("credentials never return their secret", async ({ page }) => {
  const t = tag();
  const secret = `ghp_e2e${t}SECRETvalue`;
  const bodies: Promise<string>[] = [];
  page.on("response", (res) => {
    const path = new URL(res.url()).pathname;
    // The inbox stream never ends: its body would never resolve.
    if (path.startsWith("/api/") && path !== "/api/v1/inbox/stream") {
      bodies.push(res.text().catch(() => ""));
    }
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
  const all = await Promise.all(bodies);
  expect(all.length).toBeGreaterThan(3);
  for (const b of all) expect(b).not.toContain(secret);
});

// 001-AC22: the actor filter lists who acted, and bob sees alice's entries on the shared watch.
test("audit shows the actor filter and shared-watch entries", async ({
  page,
  browser,
  baseURL,
}) => {
  const t = tag();
  const id = await createWatch(page, `Audited ${t}`);
  const call = await api(page);
  expect(
    (
      await call("POST", `/api/v1/watches/${id}/shares`, {
        email: "bob@example.test",
        permission: "ro",
      })
    ).status(),
  ).toBe(201);

  await page.goto("/audit");
  await expect(page.getByLabel("Actor").getByRole("option", { name: ALICE.name })).toHaveCount(1);

  const bob = await bobPage(browser, baseURL);
  await bob.page.goto("/audit");
  await bob.page.getByLabel("Actor").selectOption({ label: ALICE.name });
  await bob.page.getByLabel("Target type").selectOption("watch");
  const table = bob.page.getByRole("table", { name: "Audit entries, newest first" });
  const created = table
    .getByRole("row")
    .filter({ hasText: "watch.created" })
    .filter({ hasText: id.slice(0, 8) });
  await expect(created).toHaveCount(1);
  await expect(created).toContainText(ALICE.name);
  expect(bob.violations).toEqual([]);
  await bob.close();
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

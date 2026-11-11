import { BOB_AUTH_FILE } from "./auth-file";
import { api, expect, SEED, test, untilNotTooSoon, UPSTREAM } from "./fixtures";

// Bob: his seeded watch has events; the rule matches none, so routing never sends to these channels.
// Tall: the screenshots of the Filter section must not stitch the sticky header in.
test.use({ storageState: BOB_AUTH_FILE, viewport: { width: 1280, height: 2000 } });

const SHOTS = process.env.E2E_SHOTS_DIR;

interface Received {
  endpoint: string;
  body: string;
}

// 013-AC10, 013-AC11: from the edit page, a preview row's event goes to the saved rule's webhook and
// script channels; the form lists both outcomes and the script's log line.
test("live test a rule from its preview", async ({ page }) => {
  const t = `${Date.now().toString(36)}${Math.floor(Math.random() * 1e4)}`;
  const call = await api(page);
  const made: string[] = [];
  const create = async (path: string, data: object) => {
    const res = await call("POST", path, data);
    expect(res.status(), await res.text()).toBe(201);
    const { id } = (await res.json()) as { id: string };
    made.unshift(`${path}/${id}`);
    return id;
  };
  try {
    const watches = (await (await call("GET", "/api/v1/watches")).json()).items as {
      id: string;
      name: string;
    }[];
    const watchId = watches.find((w) => w.name === SEED.watch)?.id;
    expect(watchId).toBeDefined();
    const scriptId = await create("/api/v1/scripts", {
      name: `Live ${t}`,
      source: `function main(event, ctx) {\n  log("live", event.summary, ctx.test);\n}\n`,
    });
    const hook = await create("/api/v1/channels", {
      name: `Hook ${t}`,
      type: "webhook",
      config: { url: `${UPSTREAM}/_hooks/live-${t}` },
    });
    const script = await create("/api/v1/channels", {
      name: `Script ${t}`,
      type: "script",
      config: { script_id: scriptId },
    });
    const rule = await create("/api/v1/rules", {
      name: `Live ${t}`,
      watch_ids: [watchId],
      filter: 'event.type == "none"',
      channel_ids: [hook, script],
    });

    await page.goto(`/rules/${rule}`);
    const filterSection = page.getByRole("region", { name: "Filter", exact: true });
    await page.getByRole("button", { name: "Test against last 20 events" }).click();
    const first = page.getByRole("button", { name: /^Live test: / }).first();
    await expect(first).toHaveAttribute("title", "Sends this event to the rule's channels now");
    if (SHOTS) await filterSection.screenshot({ path: `${SHOTS}/013-preview.png` });

    const results = page.getByRole("status", { name: "Live test results" });
    const res = await untilNotTooSoon(page, /\/api\/v1\/rules\/[^/]+\/test$/, () => first.click());
    expect(res.status()).toBe(200);
    await expect(results.getByText(/^Live test of /)).toBeVisible();
    const items = results.getByRole("listitem");
    await expect(items.filter({ hasText: `Hook ${t}` })).toContainText("Sent");
    const run = items.filter({ hasText: `Script ${t}` });
    await expect(run).toContainText("Sent");
    await run.getByText("Logs · 1 line").click();
    await expect(run).toContainText("live [test] ");
    await expect(run).toContainText(" true");
    if (SHOTS) await filterSection.screenshot({ path: `${SHOTS}/013-results.png` });
    // A phone scrolls the table to the button: the results stay in view.
    await page.setViewportSize({ width: 390, height: 900 });
    await first.scrollIntoViewIfNeeded();
    await expect(results.getByText(/^Live test of /)).toBeInViewport();

    const got = ((await (await page.request.get(`${UPSTREAM}/_received`)).json()) as Received[])
      .filter((r) => r.endpoint === `live-${t}`)
      .map((r) => JSON.parse(r.body) as Record<string, unknown>);
    expect(got).toHaveLength(1);
    expect(got[0]).toMatchObject({ test: true, delivery_id: 0, rule: { id: rule } });
  } finally {
    for (const path of made) expect.soft((await call("DELETE", path)).ok(), path).toBe(true);
  }
});

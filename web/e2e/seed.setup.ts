import { api, expect, setFixture, SEED, test, UPSTREAM } from "./fixtures";

// Gives bob unread inbox items for the a11y, 375 px and keyboard specs. The fixture set is global, so
// this runs in its own project, before the parallel specs; activity.spec.ts uses alice's inbox.
test("seed bob's inbox", async ({ page }) => {
  await page.goto("/inbox");
  await setFixture(page, "a");
  const call = await api(page);
  // Idempotent: a previous seed (a retry, a reused database) is removed first, its inbox items with it.
  const list = async (path: string) =>
    ((await (await call("GET", path)).json()).items as { id: string; name: string }[]) ?? [];
  for (const r of (await list("/api/v1/rules")).filter((r) => r.name === SEED.rule)) {
    expect((await call("DELETE", `/api/v1/rules/${r.id}`)).ok()).toBe(true);
  }
  for (const w of (await list("/api/v1/watches")).filter((w) => w.name === SEED.watch)) {
    expect((await call("DELETE", `/api/v1/watches/${w.id}`)).ok()).toBe(true);
  }
  const unread = async () =>
    (await (await call("GET", "/api/v1/inbox/count")).json()).unread as number;
  const before = await unread();
  const res = await call("POST", "/api/v1/watches", {
    name: SEED.watch,
    kind: "github_release",
    schedule: "1h",
    config: { repo: "cli/cli", include_prereleases: true, api_url: UPSTREAM },
  });
  expect(res.status()).toBe(201);
  const { id } = (await res.json()) as { id: string };
  await expect
    .poll(async () => (await (await call("GET", `/api/v1/watches/${id}`)).json()).last_status, {
      timeout: 15_000,
    })
    .toBe("ok");
  // No filter, no channel: every event of the watch lands in the inbox.
  const rule = await call("POST", "/api/v1/rules", { name: SEED.rule, watch_id: id });
  expect(rule.status()).toBe(201);
  await setFixture(page, "b");
  expect((await call("POST", `/api/v1/watches/${id}/run`)).status()).toBe(202);
  // The fixture's 3 changes, unfiltered: exactly 3 new unread items.
  await expect.poll(unread, { timeout: 15_000 }).toBe(before + 3);
  await setFixture(page, "a");
});

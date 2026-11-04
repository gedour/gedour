import type { Page } from "@playwright/test";

import { expect, test } from "./fixtures";

// scripts/e2e.sh starts cmd/fakeupstream there, allowlisted for the worker.
const UPSTREAM = process.env.E2E_UPSTREAM_URL ?? "http://127.0.0.1:18091";
const tag = () => `${Date.now().toString(36)}${Math.floor(Math.random() * 1e4)}`;

async function api(page: Page) {
  const { csrf_token } = await (await page.request.get("/api/v1/me")).json();
  return (method: string, path: string, data?: unknown) =>
    page.request.fetch(path, { method, data, headers: { "X-CSRF-Token": csrf_token } });
}

async function setFixture(page: Page, set: "a" | "b") {
  expect((await page.request.post(`${UPSTREAM}/_fixture/${set}`)).status()).toBe(204);
}

// 001-AC7, 001-AC8, 001-AC12: the worker records a baseline, then after the upstream changes and
// "Run now", the timeline shows exactly the fixture's events.
test("the worker turns upstream changes into timeline events", async ({ page }) => {
  const t = tag();
  await page.goto("/watches");
  await setFixture(page, "a");
  const call = await api(page);
  const create = async (body: object) => {
    const res = await call("POST", "/api/v1/watches", { schedule: "1h", ...body });
    expect(res.status()).toBe(201);
    return (await res.json()).id as string;
  };
  const releases = await create({
    name: `CLI releases ${t}`,
    kind: "github_release",
    config: { repo: "cli/cli", include_prereleases: true, api_url: UPSTREAM },
  });
  const tags = await create({
    name: `Postgres tags ${t}`,
    kind: "oci_tags",
    config: {
      registry: UPSTREAM.replace("http://", ""),
      repository: "library/postgres",
      tag_regex: "^\\d+(\\.\\d+)?$",
      semver_only: true,
    },
  });
  // Both baselines in: the worker polls every 500 ms in e2e.
  for (const id of [releases, tags]) {
    await expect
      .poll(async () => (await (await call("GET", `/api/v1/watches/${id}`)).json()).last_status, {
        timeout: 15_000,
      })
      .toBe("ok");
  }

  await page.goto(`/watches/${releases}`);
  const status = page.getByRole("region", { name: "Status" });
  await expect(status).toContainText("OK");
  await expect(page.getByText("No changes yet")).toBeVisible();
  await page.getByRole("tab", { name: "State" }).click();
  await expect(page.getByRole("table")).toContainText("v2.104.0-rc.1");

  await setFixture(page, "b");
  await page.getByRole("tab", { name: "Events" }).click();
  await status.getByRole("button", { name: "Run now" }).click();
  const events = page.getByRole("list", { name: "Events" });
  await expect(events).toBeVisible({ timeout: 15_000 });
  await expect(events.getByRole("listitem")).toHaveText([
    /v2\.105\.0 published/,
    /v2\.103\.3 published/,
    /v2\.104\.0-rc\.1 promoted to release/,
  ]);
  await expect(status.getByRole("button", { name: "Run now" })).toBeEnabled();

  // The registry watch goes through the token dance on the fake registry.
  expect((await call("POST", `/api/v1/watches/${tags}/run`)).status()).toBe(202);
  await page.goto(`/watches/${tags}`);
  await expect(async () => {
    await page.reload();
    await expect(page.getByRole("list", { name: "Events" }).getByRole("listitem")).toHaveText(
      [/tag 18\.1 created \(new highest\)/, /tag 17\.6 created(?! \(new highest)/],
      { timeout: 1000 },
    );
  }).toPass({ timeout: 15_000 });
  await page.getByRole("tab", { name: "State" }).click();
  await page.getByLabel("Search tags").fill("18");
  await expect(page.getByRole("list", { name: "Tags" }).getByRole("listitem")).toHaveText([
    "18.1",
    "18.0",
  ]);

  await page.goto("/watches");
  await page.getByLabel("Search").fill(t);
  await expect(page.getByRole("list", { name: "Watches" }).getByRole("listitem")).toHaveCount(2);
  await expect(page.getByRole("list", { name: "Watches" })).not.toContainText("Pending first run");
});

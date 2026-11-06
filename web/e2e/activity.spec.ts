import { createHmac, timingSafeEqual } from "node:crypto";

import { api, expect, setFixture, TALK_SECRET, test, UPSTREAM } from "./fixtures";

// Both tests switch the fake upstream's fixture set, a global: they must not run side by side.
test.describe.configure({ mode: "serial" });

const MAILPIT = process.env.E2E_MAILPIT_URL ?? "http://127.0.0.1:18026";
const tag = () => `${Date.now().toString(36)}${Math.floor(Math.random() * 1e4)}`;

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
    kind: "github_repo",
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

interface Received {
  endpoint: string;
  header: Record<string, string>;
  body: string;
}

// 001-AC13, 001-AC14, 001-AC15, 001-AC16, 001-AC19, 001-AC20: a rule with a filter sends a watch's
// new release to the inbox (live), a signed webhook, an email and a failing webhook that retries.
test("a rule routes upstream changes to the inbox and the channels", async ({ page }) => {
  const t = tag();
  await setFixture(page, "a");
  await page.goto("/inbox");
  const call = await api(page);
  const res = await call("POST", "/api/v1/watches", {
    name: `Routed CLI ${t}`,
    kind: "github_repo",
    schedule: "1h",
    config: { repo: "cli/cli", include_prereleases: true, api_url: UPSTREAM },
  });
  expect(res.status()).toBe(201);
  const watch = (await res.json()) as { id: string; name: string };
  await expect
    .poll(
      async () => (await (await call("GET", `/api/v1/watches/${watch.id}`)).json()).last_status,
      {
        timeout: 15_000,
      },
    )
    .toBe("ok");

  // The webhook through the UI: its signing secret shows once.
  await page.goto("/channels");
  await page.getByRole("button", { name: "New channel" }).first().click();
  const dialog = page.getByRole("dialog", { name: "New channel" });
  await dialog.getByLabel("Name").fill(`Hook ${t}`);
  await dialog.getByLabel("URL").fill(`${UPSTREAM}/_hooks/ok-${t}`);
  await dialog.getByRole("button", { name: "Create channel" }).click();
  const signing = await dialog
    .getByTestId("signing-secret")
    .getByLabel("Signing secret")
    .inputValue();
  expect(signing.length).toBeGreaterThan(40);
  await dialog.getByRole("button", { name: "I have copied it" }).click();
  await expect(page.getByText(signing)).toHaveCount(0);

  const mailTo = `ops-${t}@e2e.test`;
  for (const body of [
    { name: `Mail ${t}`, type: "email", config: { to: [mailTo] } },
    { name: `Broken ${t}`, type: "webhook", config: { url: `${UPSTREAM}/_hooks/fail-${t}` } },
  ]) {
    expect((await call("POST", "/api/v1/channels", body)).status()).toBe(201);
  }

  // 001-AC13: a syntax error is shown with its position before anything is saved.
  await page.goto("/rules/new");
  await page.getByLabel("Name").fill(`Releases ${t}`);
  await page.getByLabel("Watches").selectOption({ label: watch.name });
  const expression = page.getByLabel("Expression");
  await expression.fill('event.type == "release.published" &&');
  await expect(page.getByRole("alert")).toContainText("(line 1, column");
  await expect(page.getByTestId("filter-marker")).toBeVisible();
  await expression.fill('event.type == "release.published"');
  await expect(page.getByText("Valid filter")).toBeVisible();
  for (const name of [`Hook ${t}`, `Mail ${t}`, `Broken ${t}`]) {
    await page.getByRole("checkbox", { name: new RegExp(name) }).check();
  }
  await page.getByRole("button", { name: "Create rule" }).click();
  await expect(page).toHaveURL(/\/rules$/);
  await expect(page.getByRole("list", { name: "Rules" })).toContainText(`Releases ${t}`);

  // 001-AC19: the inbox stays open (no reload) while the worker finds the change.
  await page.getByRole("link", { name: /^Inbox/ }).click();
  await expect(page.getByRole("heading", { level: 1, name: "Inbox" })).toBeVisible();
  await setFixture(page, "b");
  expect((await call("POST", `/api/v1/watches/${watch.id}/run`)).status()).toBe(202);
  // Routed (the worker and router polls take their time), then visible on the open page within 2 s.
  await expect
    .poll(
      async () => (await (await call("GET", "/api/v1/inbox?state=unread")).json()).items.length,
      {
        timeout: 15_000,
      },
    )
    .toBe(2);
  const today = page.getByRole("list", { name: "Inbox, Today" });
  await expect(today).toBeVisible({ timeout: 2000 });
  // Filtered: the promotion of the rc is not a release.published event.
  await expect(today.getByRole("listitem")).toHaveText(
    [/v2\.105\.0 published/, /v2\.103\.3 published/],
    { timeout: 2000 },
  );
  await expect(page.getByRole("link", { name: "Inbox, 2 unread" })).toBeVisible();

  // 001-AC16: the receiver verifies the signature over the raw body.
  let hooks: Received[] = [];
  await expect
    .poll(
      async () => {
        const all = (await (await page.request.get(`${UPSTREAM}/_received`)).json()) as Received[];
        hooks = all.filter((r) => r.endpoint === `ok-${t}`);
        return hooks.length;
      },
      { timeout: 15_000 },
    )
    .toBe(2);
  for (const h of hooks) {
    const want = Buffer.from(
      "sha256=" + createHmac("sha256", signing).update(h.body).digest("hex"),
    );
    expect(timingSafeEqual(Buffer.from(h.header["X-Gedour-Signature"] ?? ""), want)).toBe(true);
    expect(h.header["X-Gedour-Event"]).toBe("release.published");
    expect(JSON.parse(h.body).rule.name).toBe(`Releases ${t}`);
  }

  // The email reached mailpit.
  await expect
    .poll(
      async () => {
        const { messages } = (await (
          await page.request.get(`${MAILPIT}/api/v1/messages?limit=200`)
        ).json()) as {
          messages: { Subject: string; To: { Address: string }[] }[];
        };
        return messages
          .filter((m) => m.To.some((a) => a.Address === mailTo))
          .map((m) => m.Subject)
          .sort();
      },
      { timeout: 15_000 },
    )
    .toEqual([`${watch.name}: v2.103.3 published`, `${watch.name}: v2.105.0 published`]);

  // 001-AC20: bulk read, then bulk archive.
  await page.getByRole("checkbox", { name: "Select all" }).check();
  await page.getByRole("button", { name: "Mark read" }).click();
  await expect(page.getByRole("link", { name: /^Inbox, \d+ unread$/ })).toHaveCount(0);
  await expect(page.getByTestId("unread-badge")).toHaveCount(0);
  await page.getByRole("checkbox", { name: "Select all" }).check();
  await page.getByRole("button", { name: "Archive", exact: true }).click();
  await expect(page.getByRole("region", { name: "Nothing to read yet" })).toBeVisible();
  await page.getByRole("button", { name: "Archived" }).click();
  await expect(page.getByRole("list", { name: "Inbox, Today" })).toContainText(
    "v2.105.0 published",
  );

  // 001-AC15: the failing webhook's deliveries show the failure and the scheduled retry.
  await page.goto("/channels");
  await page.getByLabel("Channel", { exact: true }).selectOption({ label: `Broken ${t}` });
  const table = page.getByRole("table", { name: "Deliveries" });
  await expect(async () => {
    await expect(table.getByRole("row").filter({ hasText: "Retrying" })).toHaveCount(2, {
      timeout: 1000,
    });
  }).toPass({ timeout: 15_000 });
  const row = table.getByRole("row").filter({ hasText: "v2.105.0 published" });
  await expect(row).toContainText("1 / 5");
  await expect(row).toContainText("HTTP 500");
  await expect(row.getByRole("button", { name: "Retry" })).toBeVisible();
});

// 001-AC17: a script channel runs on routed events in the sandbox: its http.fetch reaches the
// (allowlisted) receiver and the delivery shows the script's logs.
test("a script channel runs its script on a routed event", async ({ page }) => {
  const t = tag();
  await setFixture(page, "a");
  await page.goto("/channels");
  const call = await api(page);
  const res = await call("POST", "/api/v1/watches", {
    name: `Scripted CLI ${t}`,
    kind: "github_repo",
    schedule: "1h",
    config: { repo: "cli/cli", include_prereleases: true, api_url: UPSTREAM },
  });
  expect(res.status()).toBe(201);
  const watch = (await res.json()) as { id: string; name: string };
  await expect
    .poll(
      async () => (await (await call("GET", `/api/v1/watches/${watch.id}`)).json()).last_status,
      { timeout: 15_000 },
    )
    .toBe("ok");
  const created = await call("POST", "/api/v1/scripts", {
    name: `Forward ${t}`,
    source: `function main(event, ctx) {
  log("forwarding", event.summary);
  const r = http.fetch("${UPSTREAM}/_hooks/script-${t}", {method: "POST", headers: {"Content-Type": "application/json"},
    body: JSON.stringify({text: ctx.watch.name + ": " + event.summary, rule: ctx.rule.name})});
  log("receiver", r.status);
}`,
  });
  expect(created.status()).toBe(201);

  // The channel through the UI.
  await page.reload();
  await page.getByRole("button", { name: "New channel" }).first().click();
  const dialog = page.getByRole("dialog", { name: "New channel" });
  await dialog.getByText("Script", { exact: true }).click();
  await dialog.getByLabel("Name").fill(`Scripted ${t}`);
  await dialog.getByRole("combobox", { name: "Script" }).selectOption({ label: `Forward ${t}` });
  await dialog.getByRole("button", { name: "Create channel" }).click();
  await expect(dialog).toBeHidden();
  const channels = (await (await call("GET", "/api/v1/channels")).json()).items as {
    id: string;
    name: string;
  }[];
  const channel = channels.find((c) => c.name === `Scripted ${t}`);
  expect(channel).toBeDefined();
  const rule = await call("POST", "/api/v1/rules", {
    name: `Scripted ${t}`,
    watch_id: watch.id,
    filter: 'event.type == "release.published"',
    channel_ids: [channel?.id],
  });
  expect(rule.status()).toBe(201);

  await setFixture(page, "b");
  expect((await call("POST", `/api/v1/watches/${watch.id}/run`)).status()).toBe(202);
  let got: Received[] = [];
  await expect
    .poll(
      async () => {
        const all = (await (await page.request.get(`${UPSTREAM}/_received`)).json()) as Received[];
        got = all.filter((r) => r.endpoint === `script-${t}`);
        return got.length;
      },
      { timeout: 20_000 },
    )
    .toBe(2);
  expect(got.map((r) => JSON.parse(r.body).text).sort()).toEqual([
    `${watch.name}: v2.103.3 published`,
    `${watch.name}: v2.105.0 published`,
  ]);
  expect(JSON.parse(got[0]?.body ?? "{}").rule).toBe(`Scripted ${t}`);

  await page.getByLabel("Channel", { exact: true }).selectOption({ label: `Scripted ${t}` });
  const table = page.getByRole("table", { name: "Deliveries" });
  await expect(async () => {
    await expect(table.getByRole("row").filter({ hasText: "Sent" })).toHaveCount(2, {
      timeout: 1000,
    });
  }).toPass({ timeout: 15_000 });
  const row = table.getByRole("row").filter({ hasText: "v2.105.0 published" });
  await row.getByText("Logs · 2 lines").click();
  await expect(row).toContainText("forwarding v2.105.0 published");
  await expect(row).toContainText("receiver 200");
});

// 003-AC2, 003-AC3: routed events reach Talk in a request the fake's signature check accepts; a wrong
// secret (401) and a missing room (404) end at once with the reason; a 429 stays scheduled for retry.
test("a rule routes upstream changes to Nextcloud Talk", async ({ page }) => {
  const t = tag();
  await setFixture(page, "a");
  await page.goto("/channels");
  const call = await api(page);
  const res = await call("POST", "/api/v1/watches", {
    name: `Talk CLI ${t}`,
    kind: "github_repo",
    schedule: "1h",
    config: { repo: "cli/cli", include_prereleases: true, api_url: UPSTREAM },
  });
  expect(res.status()).toBe(201);
  const watch = (await res.json()) as { id: string; name: string };
  await expect
    .poll(
      async () => (await (await call("GET", `/api/v1/watches/${watch.id}`)).json()).last_status,
      { timeout: 15_000 },
    )
    .toBe("ok");

  const channels: Record<string, string> = {};
  for (const [name, conversation, secret, silent] of [
    ["ok", `room${t}`, TALK_SECRET, true],
    ["badsecret", `wrong${t}`, "x".repeat(40), false],
    ["gone", `force404${t}`, TALK_SECRET, false],
    ["busy", `force429${t}`, TALK_SECRET, false],
  ] as const) {
    const created = await call("POST", "/api/v1/channels", {
      name: `Talk ${name} ${t}`,
      type: "nextcloud_talk",
      config: { url: `${UPSTREAM}/`, conversation, silent },
      secret,
    });
    expect(created.status()).toBe(201);
    channels[name] = (await created.json()).id;
  }
  const rule = await call("POST", "/api/v1/rules", {
    name: `Talk ${t}`,
    watch_id: watch.id,
    filter: 'event.type == "release.published"',
    channel_ids: Object.values(channels),
  });
  expect(rule.status()).toBe(201);

  await setFixture(page, "b");
  expect((await call("POST", `/api/v1/watches/${watch.id}/run`)).status()).toBe(202);
  let got: Received[] = [];
  await expect
    .poll(
      async () => {
        const all = (await (await page.request.get(`${UPSTREAM}/_received`)).json()) as Received[];
        got = all.filter((r) => r.endpoint === `talk-room${t}`);
        return got.length;
      },
      { timeout: 20_000 },
    )
    .toBe(2);
  const bodies = got.map(
    (r) => JSON.parse(r.body) as { message: string; referenceId: string; silent: boolean },
  );
  expect(bodies.map((b) => b.message.split("\n")[0]).sort()).toEqual([
    `**${watch.name}: v2.103.3 published**`,
    `**${watch.name}: v2.105.0 published**`,
  ]);
  expect(new Set(bodies.map((b) => b.referenceId)).size).toBe(2);
  got.forEach((r, i) => {
    const body = bodies[i];
    const random = r.header["X-Nextcloud-Talk-Bot-Random"] ?? "";
    expect(random).toMatch(/^[0-9a-f]{64}$/);
    expect(body?.referenceId).toMatch(/^[0-9a-f]{64}$/);
    expect(body?.silent).toBe(true);
    expect(r.header["X-Nextcloud-Talk-Bot-Signature"]).toBe(
      createHmac("sha256", TALK_SECRET)
        .update(random + body?.message)
        .digest("hex"),
    );
  });

  const deliveries = async (name: string) =>
    (await (await call("GET", `/api/v1/deliveries?channel_id=${channels[name]}`)).json()).items as {
      status: string;
      attempts: number;
      last_error: string | null;
    }[];
  for (const [name, status, reason] of [
    ["ok", "sent", null],
    ["gone", "dead", "HTTP 404"],
    ["busy", "failed", "HTTP 429"],
  ] as const) {
    await expect
      .poll(async () => (await deliveries(name)).map((d) => d.status), { timeout: 20_000 })
      .toEqual([status, status]);
    for (const d of await deliveries(name)) {
      expect(d.attempts).toBe(1);
      if (reason) expect(d.last_error).toContain(reason);
    }
  }
  // 003-AC8: the 401 pauses the channel: the other delivery waits without using an attempt.
  await expect
    .poll(async () => (await deliveries("badsecret")).map((d) => d.status).sort(), {
      timeout: 20_000,
    })
    .toEqual(["dead", "failed"]);
  const bad = await deliveries("badsecret");
  expect(bad.map((d) => d.attempts).sort()).toEqual([0, 1]);
  expect(bad.find((d) => d.status === "dead")?.last_error).toMatch(/: HTTP 401$/);
  expect(bad.find((d) => d.status === "failed")?.last_error).toContain("paused");
  const refused = (await (await page.request.get(`${UPSTREAM}/_received`)).json()) as Received[];
  expect(refused.filter((r) => r.endpoint === `talk-wrong${t}-rejected`)).toHaveLength(1);

  // 003-AC3: the UI shows the permanent failure with its reason, no retry scheduled.
  await page.goto("/channels");
  await page.getByLabel("Channel", { exact: true }).selectOption({ label: `Talk badsecret ${t}` });
  const row = page
    .getByRole("table", { name: "Deliveries" })
    .getByRole("row")
    .filter({ hasText: "1 / 5" });
  await expect(row).toContainText("Failed");
  await expect(row).toContainText("HTTP 401");
  await expect(row).not.toContainText("Retrying");
  await expect(row.getByRole("button", { name: "Retry" })).toBeVisible();
});

// 004-AC1, 004-AC4, 004-AC5, 004-AC7, 004-AC8: rules on payload.cves and on a pattern in payload.body each route the
// right fixture releases and nothing else, on both forges; the panel shows the ids and collapses the notes.
test("rules on release notes route the right releases", async ({ page }) => {
  const t = tag();
  await page.goto("/inbox");
  await setFixture(page, "a");
  const call = await api(page);
  const post = async (path: string, body: object) => {
    const res = await call("POST", path, body);
    expect(res.status(), await res.text()).toBe(201);
    return (await res.json()) as { id: string };
  };
  const watches = [
    await post("/api/v1/watches", {
      name: `Notes GitHub ${t}`,
      kind: "github_repo",
      schedule: "1h",
      config: { repo: "cli/cli", include_prereleases: true, api_url: UPSTREAM },
    }),
    await post("/api/v1/watches", {
      name: `Notes GitLab ${t}`,
      kind: "gitlab_release",
      schedule: "1h",
      config: { project: "gitlab-org/gitlab-runner", base_url: UPSTREAM },
    }),
  ];
  for (const { id } of watches) {
    await expect
      .poll(async () => (await (await call("GET", `/api/v1/watches/${id}`)).json()).last_status, {
        timeout: 15_000,
      })
      .toBe("ok");
  }
  const rules: Record<string, { filter: string; want: string[] }> = {
    any: {
      filter: "len(event.payload.cves) > 0",
      want: ["v19.3.5", "v19.3.8", "v2.103.3", "v2.105.0"],
    },
    one: { filter: '"CVE-2099-1234" in event.payload.cves', want: ["v2.103.3"] },
    breaking: {
      filter: 'event.payload.body matches "(?i)breaking.change"',
      want: ["v19.3.8", "v19.4.0", "v2.104.0-rc.1", "v2.105.0"],
    },
  };
  const ruleIds: string[] = [];
  for (const [name, r] of Object.entries(rules)) {
    const channel = await post("/api/v1/channels", {
      name: `${name} ${t}`,
      type: "webhook",
      config: { url: `${UPSTREAM}/_hooks/notes-${name}-${t}` },
    });
    for (const w of watches) {
      const rule = await post("/api/v1/rules", {
        name: `${name} ${w.id}`,
        watch_id: w.id,
        filter: r.filter,
        channel_ids: [channel.id],
      });
      ruleIds.push(rule.id);
    }
  }

  await setFixture(page, "b");
  for (const { id } of watches) {
    expect((await call("POST", `/api/v1/watches/${id}/run`)).status()).toBe(202);
  }
  const received = async (name: string) =>
    ((await (await page.request.get(`${UPSTREAM}/_received`)).json()) as Received[])
      .filter((r) => r.endpoint === `notes-${name}-${t}`)
      .map(
        (r) => JSON.parse(r.body).event.payload as { tag: string; body: string; cves: string[] },
      );
  for (const [name, r] of Object.entries(rules)) {
    await expect
      .poll(async () => (await received(name)).map((p) => p.tag).sort(), { timeout: 15_000 })
      .toEqual(r.want);
  }
  // 004-AC8: a webhook carries both fields.
  const cited = (await received("one"))[0];
  expect(cited?.cves).toEqual(["CVE-2099-1234", "CVE-2099-20871"]);
  expect(cited?.body).toContain("cve-2099-1234");

  // Only what a rule picked reached the inbox: not the routine release, not the one without notes.
  const inbox = (await (await call("GET", "/api/v1/inbox?state=all&limit=200")).json()).items as {
    event: { payload: { tag: string } };
    watch: { id: string };
  }[];
  expect(
    inbox
      .filter((i) => watches.some((w) => w.id === i.watch.id))
      .map((i) => i.event.payload.tag)
      .sort(),
  ).toEqual(["v19.3.5", "v19.3.8", "v19.4.0", "v2.103.3", "v2.104.0-rc.1", "v2.105.0"]);

  await setFixture(page, "a");
  await page.goto("/inbox?state=all");
  await page
    .getByRole("button", { name: /v2\.103\.3 published/ })
    .first()
    .click();
  const panel = page.getByRole("complementary", { name: "v2.103.3 published" });
  await expect(panel.getByRole("region", { name: "Details" })).toContainText(
    "CVE-2099-1234, CVE-2099-20871",
  );
  const notes = panel.locator("details");
  await expect(notes).not.toHaveAttribute("open");
  await expect(panel.getByText("## Security")).toBeHidden();
  await notes.getByText("Release notes").click();
  await expect(panel.getByText("## Security", { exact: false })).toBeVisible();

  // Alice's inbox is back as it was for a retry or a rerun.
  for (const id of ruleIds) await call("DELETE", `/api/v1/rules/${id}`);
  for (const { id } of watches) await call("DELETE", `/api/v1/watches/${id}`);
});

// 008-AC5, 008-AC6, 008-AC14: both boxes on, the baseline is quiet; fixture B's new issue and new PR show in the
// timeline, and a rule on the labels sends the issue, not the PR, to the inbox.
test("a repository watch reports new issues and PRs", async ({ page }) => {
  const t = tag();
  await page.goto("/inbox");
  await setFixture(page, "a");
  const call = await api(page);
  const res = await call("POST", "/api/v1/watches", {
    name: `Items ${t}`,
    kind: "github_repo",
    schedule: "1h",
    config: {
      repo: "cli/cli",
      include_prereleases: true,
      api_url: UPSTREAM,
      new_issues: true,
      new_prs: true,
    },
  });
  expect(res.status()).toBe(201);
  const { id } = (await res.json()) as { id: string };
  await expect
    .poll(async () => (await (await call("GET", `/api/v1/watches/${id}`)).json()).last_status, {
      timeout: 15_000,
    })
    .toBe("ok");
  const rule = await call("POST", "/api/v1/rules", {
    name: `Bugs ${t}`,
    watch_id: id,
    filter: '"bug" in event.payload.labels',
  });
  expect(rule.status()).toBe(201);
  const ruleId = ((await rule.json()) as { id: string }).id;

  await setFixture(page, "b");
  await page.goto(`/watches/${id}`);
  await expect(page.getByText("No changes yet")).toBeVisible();
  await page
    .getByRole("region", { name: "Status" })
    .getByRole("button", { name: "Run now" })
    .click();
  await expect(page.getByRole("list", { name: "Events" }).getByRole("listitem")).toHaveText(
    [
      /PR #9514 opened by dependabot\[bot\]/,
      /Issue #9513 opened by alice-q/,
      /v2\.105\.0 published/,
      /v2\.103\.3 published/,
      /v2\.104\.0-rc\.1 promoted to release/,
    ],
    { timeout: 15_000 },
  );

  const routed = async () =>
    (
      (await (await call("GET", "/api/v1/inbox?state=all&limit=200")).json()).items as {
        event: { type: string; payload: { number?: number } };
        watch: { id: string };
      }[]
    )
      .filter((i) => i.watch.id === id)
      .map((i) => `${i.event.type} ${i.event.payload.number}`);
  await expect.poll(routed, { timeout: 15_000 }).toEqual(["issue.opened 9513"]);
  await page.goto("/inbox?state=all");
  await page.getByRole("button", { name: /Issue #9513 opened by alice-q/ }).click();
  const panel = page.getByRole("complementary", { name: "Issue #9513 opened by alice-q" });
  await expect(panel.getByRole("region", { name: "Details" })).toContainText("bug, enhancement");

  await setFixture(page, "a");
  // Alice's inbox is back as it was for a retry or a rerun.
  await call("DELETE", `/api/v1/rules/${ruleId}`);
  await call("DELETE", `/api/v1/watches/${id}`);
});

// 008-AC13, 008-AC9: a pasted issue URL picks the GitHub issue kind; the baseline is quiet, then fixture B's
// close (not planned), new label and new comment show in the timeline.
test("an issue watch reports a close and a comment", async ({ page }) => {
  const t = tag();
  await page.goto("/watches/new");
  await setFixture(page, "a");
  await page.getByLabel("Paste a URL").fill("https://github.com/cli/cli/issues/9400");
  await expect(page.getByText("Recognized: GitHub issue")).toBeVisible();
  await expect(page.getByRole("radio", { name: /GitHub issue/ })).toBeChecked();
  await expect(page.getByLabel("Repository", { exact: true })).toHaveValue("cli/cli");
  await expect(page.getByLabel("Issue number")).toHaveValue("9400");
  await page.getByLabel("API URL (optional)").fill(UPSTREAM);
  await page.getByLabel("Name").fill(`Issue ${t}`);
  await page.getByRole("button", { name: "Create watch" }).click();
  await expect(page.getByRole("heading", { level: 1, name: `Issue ${t}` })).toBeVisible();
  const id = new URL(page.url()).pathname.split("/").pop() as string;
  const call = await api(page);
  await expect
    .poll(async () => (await (await call("GET", `/api/v1/watches/${id}`)).json()).last_status, {
      timeout: 15_000,
    })
    .toBe("ok");

  await setFixture(page, "b");
  await page.reload();
  await expect(page.getByText("No changes yet")).toBeVisible();
  await page
    .getByRole("region", { name: "Status" })
    .getByRole("button", { name: "Run now" })
    .click();
  await expect(page.getByRole("list", { name: "Events" }).getByRole("listitem")).toHaveText(
    [
      /Issue #9400 commented by maintainer/,
      /Issue #9400 labels: bug, wontfix/,
      /Issue #9400 closed \(not planned\)/,
    ],
    { timeout: 15_000 },
  );
  await page.getByRole("tab", { name: "State" }).click();
  await expect(page.getByRole("tabpanel")).toContainText("closed (not planned)");

  await setFixture(page, "a");
  await call("DELETE", `/api/v1/watches/${id}`);
});

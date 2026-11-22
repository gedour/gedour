import { BOB_AUTH_FILE } from "./auth-file";
import { api, expect, SEED, test, untilNotTooSoon, UPSTREAM, watchCsp } from "./fixtures";

// Bob: the AC14 rule targets his seeded watch; it matches none, so routing never sends to its channel.
test.use({ storageState: BOB_AUTH_FILE });

const INVALID = "This link is not valid.";
const RETRY = "gedour could not be reached, try again.";

const LINK = "1.00000000-0000-0000-0000-000000000000.x";

// 021-AC12: a refusal or an empty fragment says the link is not valid, a 429, a 5xx or a network failure
// says try again; both link to the inbox. An empty fragment posts nothing.
for (const [what, link, answer, message] of [
  ["a 400", LINK, 400, INVALID],
  ["a 404", LINK, 404, INVALID],
  ["a 413", LINK, 413, INVALID],
  ["a 500", LINK, 500, RETRY],
  ["a 429", LINK, 429, RETRY],
  ["a network failure", LINK, "abort", RETRY],
  ["no fragment", "", 404, INVALID],
] as const) {
  test(`the open page tells ${what} apart`, async ({ page }) => {
    let posted = 0;
    await page.route("**/api/v1/open", (route) => {
      posted++;
      return answer === "abort"
        ? route.abort()
        : route.fulfill({
            status: answer,
            contentType: "application/problem+json",
            body: JSON.stringify({ status: answer }),
          });
    });
    await page.goto(link ? `/r#${link}` : "/r");
    const shown = page.getByRole("status");
    await expect(shown).toHaveText(`${message} Open the inbox`);
    await expect(shown.getByRole("link", { name: "Open the inbox" })).toHaveAttribute(
      "href",
      "/inbox",
    );
    expect(posted).toBe(link ? 1 : 0);
  });
}

test.describe("without JavaScript", () => {
  test.use({ javaScriptEnabled: false });

  // 021-AC12: the noscript line is the only way out, and nothing is posted.
  test("the open page links to the inbox only", async ({ page }) => {
    let posted = 0;
    await page.route("**/api/v1/open", (route) => {
      posted++;
      return route.abort();
    });
    await page.goto("/r#1.00000000-0000-0000-0000-000000000000.x");
    await expect(page.getByRole("status")).toHaveText("Opening…");
    // Playwright's text matching skips a <noscript>'s own text: match its paragraph.
    await expect(page.locator("noscript p")).toHaveText(
      "This link needs JavaScript. Open the inbox",
    );
    await expect(page.getByRole("link")).toHaveCount(1);
    await expect(page.getByRole("link", { name: "Open the inbox" })).toHaveAttribute(
      "href",
      "/inbox",
    );
    expect(posted).toBe(0);
  });
});

interface Received {
  endpoint: string;
  body: string;
}

// 021-AC14: a live test's Telegram message links upstream through /r, which a browser without a session
// follows to the event's stored url. The read mark is 021-AC7's: other bob specs open the newest item.
test("a read_on_open link opens upstream without a session", async ({ page, browser, baseURL }) => {
  const t = `${Date.now().toString(36)}${Math.floor(Math.random() * 1e4)}`;
  const chat = `-100${Date.now()}${Math.floor(Math.random() * 1e4)}`;
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
    const tg = await create("/api/v1/channels", {
      name: `Open ${t}`,
      type: "telegram",
      config: { chat_id: chat, read_on_open: true },
      secret: "123456:AAAAAAAAAAAAAAAAAAAAAAAAAAAA",
    });
    const rule = await create("/api/v1/rules", {
      name: `Open ${t}`,
      watch_ids: [watchId],
      filter: 'event.type == "none"',
      channel_ids: [tg],
    });

    await page.goto(`/rules/${rule}`);
    await page.getByRole("button", { name: "Test against last 20 events" }).click();
    const first = page.getByRole("button", { name: /^Live test: / }).first();
    const res = await untilNotTooSoon(page, /\/api\/v1\/rules\/[^/]+\/test$/, () => first.click());
    expect(res.status()).toBe(200);

    const sent = ((await (await page.request.get(`${UPSTREAM}/_received`)).json()) as Received[])
      .filter((r) => r.endpoint === "telegram")
      .map((r) => JSON.parse(r.body) as { chat_id: string; text: string })
      .filter((m) => m.chat_id === chat);
    expect(sent).toHaveLength(1);
    const href = /<a href="([^"]*\/r#[^"]*)">Open upstream<\/a>/.exec(sent[0]?.text ?? "")?.[1];
    expect(href, sent[0]?.text).toMatch(
      new RegExp(`^${baseURL}/r#\\d+\\.[0-9a-f-]{36}\\.[\\w-]+$`),
    );
    const eventId = Number((href as string).split("#")[1]?.split(".")[0]);
    const events = (await (await call("GET", `/api/v1/watches/${watchId}/events`)).json())
      .items as { id: number; payload: { url?: string } }[];
    const stored = events.find((e) => e.id === eventId)?.payload.url;
    expect(stored).toMatch(/^https:\/\/github\.com\//);

    const ctx = await browser.newContext();
    try {
      const anon = await ctx.newPage();
      const violations = await watchCsp(anon, baseURL);
      await anon.route("https://github.com/**", (route) =>
        route.fulfill({ status: 200, contentType: "text/html", body: "<p>upstream</p>" }),
      );
      const opened = anon.waitForResponse(
        (r) => r.url().endsWith("/api/v1/open") && r.request().method() === "POST",
      );
      await anon.goto(href as string);
      expect((await opened).status()).toBe(200);
      await expect(anon).toHaveURL(stored as string);
      expect(violations).toEqual([]);
    } finally {
      await ctx.close();
    }
  } finally {
    for (const path of made) expect.soft((await call("DELETE", path)).ok(), path).toBe(true);
  }
});

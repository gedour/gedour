import { BOB_AUTH_FILE } from "./auth-file";
import { api, expect, PUSH_KEYS, test, UPSTREAM } from "./fixtures";

// The headless shell reports notifications denied whatever is granted; the new headless mode does not.
// Bob: the seed leaves events in his inbox for the notification link to open.
test.use({ channel: "chromium", storageState: BOB_AUTH_FILE });

interface Received {
  endpoint: string;
  body: string;
}

// 010-AC12, 010-AC13, 010-AC17: notifications granted, picking Web Push registers this browser, Send test
// reaches the fake push service decryptable, the real service worker shows it, and its click handler
// routes the open gedour page in place to the notification's link, opening its inbox item.
// Headless Chromium cannot reach a real push service: subscribe() is stubbed to hand out a subscription
// to the fake one. The service worker, its registration and the notification are the browser's own.
test("register this browser, send a test and open the notification", async ({
  page,
  context,
  baseURL,
}) => {
  const id = `e2e${Date.now().toString(36)}${Math.floor(Math.random() * 1e4)}`;
  const endpoint = `${UPSTREAM}/push/${id}`;
  const origin = new URL(baseURL ?? "").origin;
  await context.grantPermissions(["notifications"], { origin });
  await page.addInitScript(
    ({ endpoint, p256dh, auth }) => {
      let sub: PushSubscription | null = null;
      const make = () =>
        ({
          endpoint,
          expirationTime: null,
          toJSON: () => ({ endpoint, expirationTime: null, keys: { p256dh, auth } }),
          unsubscribe: async () => true,
        }) as unknown as PushSubscription;
      PushManager.prototype.subscribe = async () => (sub = make());
      PushManager.prototype.getSubscription = async () => sub;
    },
    { endpoint, ...PUSH_KEYS },
  );

  const sw = await page.request.get("/sw.js");
  expect(sw.status()).toBe(200);
  expect(sw.headers()["content-type"]).toMatch(/javascript/);
  expect(sw.headers()["content-security-policy"]).toContain("worker-src 'self'");

  await page.goto("/channels");
  await expect(page.getByRole("form", { name: "Add this browser" })).toBeVisible();
  await page.getByRole("button", { name: "New channel" }).first().click();
  const dialog = page.getByRole("dialog", { name: "New channel" });
  const name = `Push ${id}`;
  await dialog.getByText("Web Push", { exact: true }).click();
  // The pick registered this browser: the device list names it, and the fallback is gone.
  await expect(page.getByRole("list", { name: "Push devices" })).toContainText(/This browser/);
  await expect(dialog.getByRole("button", { name: "Add this browser" })).toHaveCount(0);
  await dialog.getByLabel(/Pick devices/).check();
  await expect(dialog.getByRole("checkbox", { name: /this browser/ })).toBeChecked();
  await dialog.getByLabel(/All my devices/).check();
  await dialog.getByLabel("Name").fill(name);
  await dialog.getByRole("button", { name: "Create channel" }).click();
  await expect(dialog).toBeHidden();
  const devices = page.getByRole("list", { name: "Push devices" });
  await expect(devices.getByText("This browser")).toBeVisible();

  const card = page.getByRole("listitem").filter({ hasText: name });
  await expect(card).toContainText("Web Push · all devices");
  // Channel tests are rate limited per user, and other specs test alice's channels too.
  await expect(async () => {
    await card.getByRole("button", { name: "Send test" }).click();
    await expect(page.getByText(`Test message sent to ${name}`)).toBeVisible({ timeout: 3000 });
  }).toPass({ timeout: 30_000 });
  const got = ((await (await page.request.get(`${UPSTREAM}/_received`)).json()) as Received[]).find(
    (r) => r.endpoint === `push-${id}`,
  );
  expect(got, "the fake push service decrypted the test").toBeDefined();
  const message = JSON.parse(got?.body ?? "{}") as { title: string; url: string };
  expect(message.url).toBe("/inbox");
  await expect(devices).toContainText("Last push");

  // The browser's push path: the payload the push service would forward, delivered to the real worker.
  const cdp = await context.newCDPSession(page);
  const registrationId = new Promise<string>((resolve) =>
    cdp.on("ServiceWorker.workerRegistrationUpdated", (e) => {
      const r = e.registrations.find((x) => x.scopeURL === `${origin}/` && !x.isDeleted);
      if (r) resolve(r.registrationId);
    }),
  );
  await cdp.send("ServiceWorker.enable");
  const worker =
    context.serviceWorkers().find((w) => w.url() === `${origin}/sw.js`) ??
    (await context.waitForEvent("serviceworker"));
  /** Delivers data as a push, waits for its notification, then runs the worker's click handler on it. */
  const pushAndClick = async (data: string, tag: string) => {
    await cdp.send("ServiceWorker.deliverPushMessage", {
      origin,
      registrationId: await registrationId,
      data,
    });
    await expect
      .poll(
        () =>
          page.evaluate(async () =>
            (await (await navigator.serviceWorker.ready).getNotifications()).map((n) => n.tag),
          ),
        // showNotification resolves once the icon is fetched.
        { timeout: 15_000 },
      )
      .toContain(tag);
    await worker.evaluate(async (t) => {
      const scope = self as unknown as {
        registration: ServiceWorkerRegistration;
        openNotification: (n: Notification) => Promise<void>;
      };
      const n = (await scope.registration.getNotifications()).find((x) => x.tag === t);
      if (!n) throw new Error(`no notification ${t}`);
      await scope.openNotification(n);
    }, tag);
  };
  // A marker on the page: it survives in-place routing, never a reload.
  await page.evaluate(() => ((window as unknown as { kept: boolean }).kept = true));
  const kept = () => page.evaluate(() => (window as unknown as { kept?: boolean }).kept === true);

  await pushAndClick(got?.body ?? "", "gedour-test");
  await expect(page).toHaveURL(`${origin}/inbox`);
  await expect(page.getByRole("heading", { level: 1, name: "Inbox" })).toBeVisible();
  expect(await kept()).toBe(true);

  // A delivery's link: the seeded event's item opens, and the link leaves the URL.
  const items = (await (await (await api(page))("GET", "/api/v1/inbox")).json()).items as {
    event: { id: number; payload: { tag: string } };
  }[];
  const seeded = items[0];
  if (!seeded) throw new Error("seed.setup.ts left no inbox item");
  await page.goto("/channels");
  await page.evaluate(() => ((window as unknown as { kept: boolean }).kept = true));
  await pushAndClick(
    JSON.stringify({
      title: "Seeded event",
      body: "",
      url: `/inbox?event=${seeded.event.id}`,
      tag: `gedour-event-${seeded.event.id}`,
      silent: false,
    }),
    `gedour-event-${seeded.event.id}`,
  );
  await expect(
    page.getByRole("complementary", {
      name: new RegExp(seeded.event.payload.tag.replaceAll(".", "\\.")),
    }),
  ).toBeVisible();
  await expect(page).toHaveURL(`${origin}/inbox`);
  expect(await kept()).toBe(true);
});

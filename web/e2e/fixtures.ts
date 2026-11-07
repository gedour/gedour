import { expect, test as base, type Page } from "@playwright/test";

export const ALICE = { username: "alice", password: "alice", name: "Alice Liddell" };
export const BOB = { username: "bob", password: "bob", name: "Bob Builder" };
/** Holds no gedour client role: the realm's refused user. */
export const CAROL = { username: "carol", password: "carol", name: "Carol Nora" };
/** Disposable: admin.spec.ts purges it, so no other spec may sign in as dave. */
export const DAVE = { username: "dave", password: "dave", name: "Dave Purged" };

// Every test fails on a CSP violation on our origin (the provider's pages carry their own policy).
// Collected on the Node side so reloads and navigations cannot wipe them; the console listener is a
// second net for reports the event misses.
export const test = base.extend<{ cspViolations: string[] }>({
  cspViolations: [
    async ({ page, baseURL }, use) => {
      const violations = await watchCsp(page, baseURL);
      await use(violations);
      expect(violations).toEqual([]);
    },
    { auto: true },
  ],
});

/** Collects CSP violations on our origin for page; the fixture does it for `page`, call it for others. */
export async function watchCsp(page: Page, baseURL: string | undefined): Promise<string[]> {
  const origin = new URL(baseURL ?? "http://localhost").origin;
  const violations: string[] = [];
  await page.exposeFunction("reportCspViolation", (o: string, v: string) => {
    if (o === origin) violations.push(v);
  });
  await page.addInitScript(() => {
    document.addEventListener("securitypolicyviolation", (e) => {
      const w = window as unknown as { reportCspViolation: (o: string, v: string) => void };
      w.reportCspViolation(location.origin, `${e.violatedDirective} ${e.blockedURI}`);
    });
  });
  page.on("console", (msg) => {
    const from = msg.location().url;
    if (
      msg.type() === "error" &&
      /Content Security Policy/i.test(msg.text()) &&
      from.startsWith(origin)
    ) {
      violations.push(msg.text());
    }
  });
  return violations;
}

export { expect };

/** Fills the Keycloak login form the app redirected to, and waits to be back on the app. */
export async function signInAtProvider(page: Page, user: { username: string; password: string }) {
  await expect(page).toHaveURL(/\/realms\/gedour-test\/protocol\/openid-connect\/auth/);
  await page.getByRole("textbox", { name: "Username or email" }).fill(user.username);
  await page.getByRole("textbox", { name: "Password", exact: true }).fill(user.password);
  await page.getByRole("button", { name: /^sign in$/i }).click();
}

/** The full-screen sign-in gate (002-AC1); name is its heading, which depends on the reason. */
export const gate = (page: Page, name: string | RegExp = "Sign in to continue") =>
  page.getByRole("heading", { level: 1, name });

/** Clicks "Sign in" on the gate, then signs in at the provider. */
export async function signInFromGate(page: Page, user: { username: string; password: string }) {
  await expect(gate(page)).toBeVisible();
  await page.getByRole("link", { name: "Sign in" }).click();
  await signInAtProvider(page, user);
}

// Watches target the fake upstream that scripts/e2e.sh starts; e2e never polls the real forges.
export const UPSTREAM = process.env.E2E_UPSTREAM_URL ?? "http://127.0.0.1:18091";

/** The shared secret of the fake upstream's Talk bot (testupstream.TalkSecret). */
export const TALK_SECRET = "fake-talk-bot-shared-secret-0123456789abcdef";

/** Bob's seeded watch and rules (seed.setup.ts); 3 events in his inbox, the 2 with CVE ids high. */
export const SEED = {
  watch: "Seeded CLI releases",
  rule: "Seeded inbox rule",
  highRule: "Seeded CVE rule",
};

/** Calls the API as the page's user, with the session's CSRF token. */
export async function api(page: Page) {
  const { csrf_token } = await (await page.request.get("/api/v1/me")).json();
  return (method: string, path: string, data?: unknown) =>
    page.request.fetch(path, { method, data, headers: { "X-CSRF-Token": csrf_token } });
}

/** Switches the fake upstream's global fixture set; only seed.setup.ts and activity.spec.ts may. */
export async function setFixture(page: Page, set: "a" | "b") {
  expect((await page.request.post(`${UPSTREAM}/_fixture/${set}`)).status()).toBe(204);
}

/** Enters a rule filter until its preview reads "Valid filter"; a retry adds parentheses, so it is a new query, not a cached 429. */
export async function enterFilter(
  page: Page,
  filter: string,
  enter: (text: string, attempt: number) => Promise<void>,
) {
  let attempt = 0;
  // Parallel specs signed in as one user share its preview budget: a 429 shows "Could not check" until it refills.
  await expect(async () => {
    const text = "(".repeat(attempt) + filter + ")".repeat(attempt);
    await enter(text, attempt++);
    await expect(page.getByText("Valid filter")).toBeVisible({ timeout: 2000 });
  }).toPass({ timeout: 10_000 });
}

/** The fake push service's subscription keys (testupstream.PushP256DH, PushAuth): it decrypts what they get. */
export const PUSH_KEYS = {
  p256dh: "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4",
  auth: "BTBZMqHH6r4Tts7J_aSIgg",
};

/** Registers a push device for the page's user, at the fake push service; returns its name. */
export async function addDevice(page: Page, label: string) {
  const id = `${label.replace(/\W/g, "")}${Date.now().toString(36)}${Math.floor(Math.random() * 1e4)}`;
  const name = `${label} device ${id}`;
  const res = await (
    await api(page)
  )("POST", "/api/v1/push/devices", {
    endpoint: `${UPSTREAM}/push/${id}`,
    keys: PUSH_KEYS,
    name,
    user_agent: "Mozilla/5.0 (X11; Linux x86_64) e2e",
  });
  expect(res.status()).toBe(201);
  return name;
}

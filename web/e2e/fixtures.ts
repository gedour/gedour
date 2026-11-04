import { expect, test as base, type Page } from "@playwright/test";

export const ALICE = { username: "alice", password: "alice", name: "Alice Liddell" };
export const BOB = { username: "bob", password: "bob", name: "Bob Builder" };

// Every test fails on a CSP violation on our origin (the provider's pages carry their own policy).
// Collected on the Node side so reloads and navigations cannot wipe them; the console listener is a
// second net for reports the event misses.
export const test = base.extend<{ cspViolations: string[] }>({
  cspViolations: [
    async ({ page, baseURL }, use) => {
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
      await use(violations);
      expect(violations).toEqual([]);
    },
    { auto: true },
  ],
});

export { expect };

/** Fills the Keycloak login form the app redirected to, and waits to be back on the app. */
export async function signInAtProvider(page: Page, user: { username: string; password: string }) {
  await expect(page).toHaveURL(/\/realms\/gedour-test\/protocol\/openid-connect\/auth/);
  await page.getByRole("textbox", { name: "Username or email" }).fill(user.username);
  await page.getByRole("textbox", { name: "Password", exact: true }).fill(user.password);
  await page.getByRole("button", { name: /^sign in$/i }).click();
}

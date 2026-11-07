import type { Response } from "@playwright/test";

import { BOB_AUTH_FILE } from "./auth-file";
import { expect, test } from "./fixtures";

test.use({ storageState: BOB_AUTH_FILE, permissions: ["clipboard-read", "clipboard-write"] });

const tag = () => `${Date.now().toString(36)}${Math.floor(Math.random() * 1e4)}`;

// 007-AC32, 007-AC1: the key shows once with a working copy button and this instance's URL; no other answer holds it.
test("create, reveal, list and revoke an API key", async ({ page, baseURL }) => {
  const name = `E2E ${tag()}`;
  const bodies: Promise<{ path: string; text: string }>[] = [];
  page.on("response", (res: Response) => {
    const path = new URL(res.url()).pathname;
    // The inbox stream never ends: its body would never resolve.
    if (path.startsWith("/api/") && path !== "/api/v1/inbox/stream") {
      bodies.push(
        res
          .text()
          .catch(() => "")
          .then((text) => ({ path: `${res.request().method()} ${path}`, text })),
      );
    }
  });

  await page.goto("/keys");
  await page.getByRole("button", { name: "New API key" }).click();
  const form = page.getByRole("dialog", { name: "New API key" });
  await form.getByLabel("Name", { exact: true }).fill(name);
  await form.getByLabel("Duration", { exact: true }).selectOption("30");
  await form.getByRole("button", { name: "Create key" }).click();

  const reveal = page.getByRole("dialog", { name: `Copy ${name} now` });
  const key = await reveal.getByLabel("API key", { exact: true }).inputValue();
  expect(key).toMatch(/^gdr_[A-Za-z0-9_-]{43}$/);
  await expect(reveal.getByLabel("Client setup", { exact: true })).toHaveValue(
    `claude mcp add --scope user --transport http gedour ${baseURL}/mcp --header "Authorization: Bearer ${key}"`,
  );
  await reveal.getByRole("button", { name: "Copy API key" }).click();
  await expect(reveal.getByRole("button", { name: "Copied API key" })).toBeVisible();
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(key);
  await page.keyboard.press("Escape");
  await expect(reveal).toBeVisible();
  await reveal.getByRole("button", { name: "I have copied it" }).click();
  await expect(reveal).toBeHidden();

  const row = page.getByRole("row").filter({ hasText: name });
  // CSS capitalizes it on screen; the text itself is the API value.
  await expect(row).toContainText("read");
  await expect(row).toContainText(`gdr_${key.slice(4, 12)}…`);
  await expect(row).toContainText("active");
  expect(await page.content()).not.toContain(key);
  await page.reload();
  expect(await page.content()).not.toContain(key);

  await row.getByRole("button", { name: "Revoke" }).click();
  await page
    .getByRole("dialog", { name: `Revoke ${name}?` })
    .getByRole("button", { name: "Revoke" })
    .click();
  await expect(page.getByText(`Revoked ${name}`)).toBeVisible();
  await expect(row).toContainText("revoked");
  await expect(row.getByRole("button", { name: "Revoke" })).toHaveCount(0);

  bodies.push(
    page.request
      .get("/api/v1/audit?target_type=api_key")
      .then(async (r) => ({ path: "audit", text: await r.text() })),
  );
  const all = await Promise.all(bodies);
  const holding = all.filter((b) => b.text.includes(key)).map((b) => b.path);
  expect(holding).toEqual(["POST /api/v1/api-keys"]);
  const audit = all.find((b) => b.path === "audit")?.text ?? "";
  expect(audit).toContain("api_key.created");
  expect(audit).toContain("api_key.revoked");
  expect(audit).toContain(name);
  expect(audit).not.toContain("[redacted]");
});

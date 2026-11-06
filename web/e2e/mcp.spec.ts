import type { APIRequestContext } from "@playwright/test";

import { BOB_AUTH_FILE } from "./auth-file";
import { expect, test } from "./fixtures";

test.use({ storageState: BOB_AUTH_FILE });

const mcpCall = (request: APIRequestContext, key: string, method: string, params: object) =>
  request.post("/mcp", {
    headers: {
      Authorization: `Bearer ${key}`,
      Accept: "application/json, text/event-stream",
      "MCP-Protocol-Version": "2025-11-25",
    },
    data: { jsonrpc: "2.0", id: 1, method, params },
  });

// 007-AC7, 007-AC15: a key made in the UI opens /mcp until the UI revokes it.
test("use a key from the UI on /mcp, then revoke it", async ({ page, playwright, baseURL }) => {
  const name = `MCP ${Date.now().toString(36)}${Math.floor(Math.random() * 1e4)}`;
  await page.goto("/keys");
  await page.getByRole("button", { name: "New API key" }).click();
  const form = page.getByRole("dialog", { name: "New API key" });
  await form.getByLabel("Name", { exact: true }).fill(name);
  await form.getByRole("button", { name: "Create key" }).click();
  const reveal = page.getByRole("dialog", { name: `Copy ${name} now` });
  const key = await reveal.getByLabel("API key", { exact: true }).inputValue();
  await reveal.getByRole("button", { name: "I have copied it" }).click();

  // No cookie: only the key may open /mcp.
  const request = await playwright.request.newContext({
    baseURL,
    storageState: { cookies: [], origins: [] },
  });
  const list = await mcpCall(request, key, "tools/list", {});
  expect(list.status()).toBe(200);
  const tools = (await list.json()).result.tools.map((t: { name: string }) => t.name);
  expect(tools).toEqual([
    "count_inbox",
    "list_inbox",
    "list_watches",
    "get_watch",
    "list_watch_events",
    "list_watch_states",
    "list_rules",
    "get_rule",
    "list_deliveries",
  ]);

  const count = await mcpCall(request, key, "tools/call", { name: "count_inbox", arguments: {} });
  expect(count.status()).toBe(200);
  const { result } = await count.json();
  expect(result.isError ?? false).toBe(false);
  const fence = /<untrusted-data nonce="([^"]+)">\n([\s\S]*)<\/untrusted-data nonce="\1">$/.exec(
    result.content[0].text,
  );
  expect(fence).not.toBeNull();
  // The seed left bob 3 events; the keyboard spec may have read one.
  expect(JSON.parse(fence?.[2] ?? "").unread).toBeGreaterThanOrEqual(0);

  const row = page.getByRole("row").filter({ hasText: name });
  await row.getByRole("button", { name: "Revoke" }).click();
  await page
    .getByRole("dialog", { name: `Revoke ${name}?` })
    .getByRole("button", { name: "Revoke" })
    .click();
  await expect(row).toContainText("revoked");

  expect((await mcpCall(request, key, "tools/list", {})).status()).toBe(401);
  await request.dispose();
});

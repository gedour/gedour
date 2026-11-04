import { expect, test } from "./fixtures";

// The dry run's fetch targets the fake upstream's webhook receiver: it must see nothing.
const UPSTREAM = process.env.E2E_UPSTREAM_URL ?? "http://127.0.0.1:18091";
const tag = () => `${Date.now().toString(36)}${Math.floor(Math.random() * 1e4)}`;

// 001-AC18: a script typed into CodeMirror is highlighted, saved, refused with its position when it
// does not parse, and run on a sample event: logs, result and recorded fetches show, nothing is sent.
test("alice writes a script and dry-runs it on a sample event", async ({ page }) => {
  const t = tag();
  await page.goto("/scripts");
  await page.getByRole("link", { name: "New script" }).first().click();
  await expect(page).toHaveURL(/\/scripts\/new$/);
  await page.getByLabel("Name").fill(`Dry ${t}`);

  const editor = page.getByRole("textbox", { name: "Source" });
  await editor.click();
  await page.keyboard.press("ControlOrMeta+a");
  await page.keyboard.press("Delete");
  // Bulk text through insertText (auto-closed brackets would double up), then a line typed key by key.
  await page.keyboard.insertText(
    `function main(event, ctx) {\n` +
      `const r = http.fetch("${UPSTREAM}/_hooks/dry-${t}", {method: "POST", headers: {Authorization: "Bearer s3cret"}, body: event.summary});\n`,
  );
  await page.keyboard.type(`log("typed", event.type, r.status);`);
  await page.keyboard.insertText(`\nreturn {watch: ctx.watch.name};\n}\n`);
  // 001-AC18: highlighted as JavaScript: tokens get their own styled spans.
  await expect(editor.locator(".cm-line span", { hasText: /^function$/ }).first()).toBeVisible();
  await expect(editor.locator(".cm-line span", { hasText: /^"typed"$/ })).toBeVisible();

  await page.getByRole("button", { name: "Create script" }).click();
  await expect(page).toHaveURL(/\/scripts\/[0-9a-f-]{36}$/);
  await expect(page.getByRole("heading", { level: 1, name: `Edit Dry ${t}` })).toBeVisible();

  // A syntax error is refused on save and marked at its line and column.
  await editor.click();
  await page.keyboard.press("ControlOrMeta+End");
  await page.keyboard.insertText("let broken = = 1;\n");
  await page.getByRole("button", { name: "Save script" }).click();
  await expect(page.getByRole("alert")).toContainText(/line \d+, column \d+/);
  // Underlined in place (CodeMirror's default underline is a data: image the CSP guard would catch).
  await expect(page.locator(".cm-lintRange-error")).toHaveText("=");
  await editor.click();
  await page.keyboard.press("ControlOrMeta+z");
  await expect(page.getByRole("alert")).toHaveCount(0);
  await expect(editor).not.toContainText("broken");

  await page.getByLabel("Event").selectOption({ label: "Release published" });
  await page.getByRole("button", { name: "Run with sample event" }).click();
  const results = page.getByTestId("script-results");
  await expect(results).toContainText("Returned");
  await expect(results.getByRole("region", { name: "Logs" })).toContainText(
    "typed release.published 200",
  );
  await expect(results.getByRole("region", { name: "Result" })).toContainText(
    '{"watch":"GitHub CLI releases"}',
  );
  const fetches = results.getByRole("region", { name: "Fetches" });
  await expect(fetches).toContainText(`POST${UPSTREAM}/_hooks/dry-${t}`);
  await expect(fetches).toContainText("Authorization: [redacted]");
  await expect(fetches).not.toContainText("s3cret");

  // Nothing was sent for real.
  const received = (await (await page.request.get(`${UPSTREAM}/_received`)).json()) as {
    endpoint: string;
  }[];
  expect(received.filter((r) => r.endpoint === `dry-${t}`)).toEqual([]);

  await page.goto("/scripts");
  await expect(page.getByRole("list", { name: "Scripts" })).toContainText(`Dry ${t}`);
});

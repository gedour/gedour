import { BOB_AUTH_FILE } from "./auth-file";
import { expect, TALK_SECRET, test, UPSTREAM } from "./fixtures";

const tag = () => `${Date.now().toString(36)}${Math.floor(Math.random() * 1e4)}`;

interface Received {
  endpoint: string;
  body: string;
}

const received = async (page: import("@playwright/test").Page) =>
  (await (await page.request.get(`${UPSTREAM}/_received`)).json()) as Received[];

// 003-AC1, 003-AC2: a Talk channel from the UI: created, tested against the fake's signature check,
// edited with the secret kept, then deleted.
test("create, test, edit and delete a Nextcloud Talk channel", async ({ page }) => {
  const t = tag();
  const name = `Room ${t}`;
  await page.goto("/channels");
  await page.getByRole("button", { name: "New channel" }).first().click();
  const dialog = page.getByRole("dialog", { name: "New channel" });
  await dialog.getByText("Nextcloud Talk", { exact: true }).click();
  await dialog.getByLabel("Name").fill(name);
  await dialog.getByLabel("Nextcloud URL").fill(`${UPSTREAM}/`);
  await dialog.getByLabel("Conversation token").fill(`room${t}`);
  await dialog.getByLabel("Bot secret").fill(TALK_SECRET);
  await dialog.getByRole("switch", { name: "Silent" }).click();
  await dialog.getByRole("button", { name: "Create channel" }).click();
  await expect(dialog).toBeHidden();

  const card = page.getByRole("listitem").filter({ hasText: name });
  await expect(card).toContainText(`Nextcloud Talk · conversation room${t} on `);
  await expect(page.getByText(TALK_SECRET)).toHaveCount(0);

  await card.getByRole("button", { name: "Send test" }).click();
  await expect(page.getByText(`Test message sent to ${name}`)).toBeVisible();
  const posted = (await received(page)).filter((r) => r.endpoint === `talk-room${t}`);
  expect(posted).toHaveLength(1);
  const body = JSON.parse(posted[0]?.body ?? "{}");
  expect(body.silent).toBe(true);
  expect(body.message).toContain("test notification");

  await card.getByRole("button", { name: `Edit ${name}` }).click();
  const edit = page.getByRole("dialog", { name: /^Edit/ });
  await expect(edit.getByLabel("Bot secret")).toHaveValue("");
  await expect(edit.getByRole("switch", { name: "Silent" })).toBeChecked();
  await edit.getByRole("switch", { name: "Silent" }).click();
  await edit.getByRole("button", { name: "Save channel" }).click();
  await expect(edit).toBeHidden();

  await card.getByRole("button", { name: `Delete ${name}` }).click();
  const confirm = page.getByRole("dialog", { name: /^Delete/ });
  await confirm.getByRole("button", { name: /^Delete/ }).click();
  await expect(card).toHaveCount(0);
});

// 012-AC13, 012-AC14: the digest lives on the rule: times and zone saved, summarized in the list, shown back on
// edit, locked while the priority is high; the channel form has no delivery settings.
test("create a digest rule and see its schedule", async ({ page }) => {
  const name = `Digest ${tag()}`;
  await page.goto("/channels");
  await page.getByRole("button", { name: "New channel" }).first().click();
  const dialog = page.getByRole("dialog", { name: "New channel" });
  await dialog.getByText("Telegram", { exact: true }).click();
  await expect(dialog.getByRole("radio", { name: /^Digest/ })).toHaveCount(0);
  await dialog.getByRole("button", { name: "Cancel" }).click();

  await page.goto("/rules/new");
  await page.getByLabel("Name").fill(name);
  // Matches nothing: no inbox item nor delivery for the other specs.
  await page.getByLabel("Expression").fill('event.type == "none"');
  await page.getByRole("radio", { name: /^Digest/ }).check();
  await expect(
    page.getByText("Webhook and script channels still get each event at once"),
  ).toBeVisible();
  await page.getByRole("button", { name: "Add a time" }).click();
  await page.getByLabel("Time 2", { exact: true }).fill("18:00");
  await page.getByLabel("Time zone").fill("Europe/Paris");
  await page.getByRole("button", { name: "Create rule" }).click();
  await expect(page).toHaveURL(/\/rules$/);

  const item = page.getByRole("listitem").filter({ hasText: name });
  await expect(item).toContainText("Digest at 09:00, 18:00 (Europe/Paris)");
  await item.getByRole("link", { name }).click();
  await expect(page.getByRole("radio", { name: /^Digest/ })).toBeChecked();
  await expect(page.getByLabel("Time 2", { exact: true })).toHaveValue("18:00");
  await expect(page.getByLabel("Time zone")).toHaveValue("Europe/Paris");
  await page.getByLabel("Priority").selectOption("high");
  await expect(page.getByRole("radio", { name: /^Digest/ })).toBeDisabled();
  await expect(page.getByText("High priority sends each event at once")).toBeVisible();

  await page.getByRole("button", { name: "Delete rule" }).click();
  await page
    .getByRole("dialog", { name: /^Delete/ })
    .getByRole("button", { name: /^Delete/ })
    .click();
  await expect(page).toHaveURL(/\/rules$/);
  await expect(page.getByRole("listitem").filter({ hasText: name })).toHaveCount(0);
});

test.describe("as bob", () => {
  // The test button is rate limited per user: alice's test above must not hold this one back.
  test.use({ storageState: BOB_AUTH_FILE });

  // 003-AC4: a silent Telegram channel from the UI records disable_notification: true.
  test("a silent Telegram channel sends disable_notification", async ({ page }) => {
    const t = tag();
    const name = `Quiet ${t}`;
    const chat = `-100${Date.now()}`;
    await page.goto("/channels");
    await page.getByRole("button", { name: "New channel" }).first().click();
    const dialog = page.getByRole("dialog", { name: "New channel" });
    await dialog.getByText("Telegram", { exact: true }).click();
    await dialog.getByLabel("Name").fill(name);
    await dialog.getByLabel("Chat ID").fill(chat);
    await dialog.getByLabel("Bot token").fill("123456:AAAAAAAAAAAAAAAAAAAAAAAAAAAA");
    await dialog.getByRole("switch", { name: "Silent" }).click();
    await dialog.getByRole("button", { name: "Create channel" }).click();
    await expect(dialog).toBeHidden();
    await page
      .getByRole("listitem")
      .filter({ hasText: name })
      .getByRole("button", { name: "Send test" })
      .click();
    await expect(page.getByText(`Test message sent to ${name}`)).toBeVisible();
    const sent = (await received(page))
      .filter((r) => r.endpoint === "telegram")
      .map((r) => JSON.parse(r.body) as { chat_id: string; disable_notification: boolean })
      .filter((m) => m.chat_id === chat);
    expect(sent).toHaveLength(1);
    expect(sent[0]?.disable_notification).toBe(true);
  });
});

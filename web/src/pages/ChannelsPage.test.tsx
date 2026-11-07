import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { CHANNEL_TYPES, mockApi } from "@/test/api";
import { renderApp } from "@/test/render";
import { channel, delivery, script } from "@/test/routing";
import { lastCall, pending, problem } from "@/test/watches";
import type { Channel } from "@/types/api";

const GENERATED = "FmI9sOssGHJCLR406IiNSNAHPEVSeOeWkKpz6yUZ3fA";

describe("ChannelsPage", () => {
  it("lists channels with their type, target and last delivery", async () => {
    mockApi({
      "/api/v1/channels": {
        items: [
          channel({ last_delivery_status: "dead", last_delivery_at: new Date().toISOString() }),
          channel({
            id: "c2",
            name: "Ops",
            type: "email",
            config: { to: ["ops@x.test", "dev@x.test"] },
          }),
          channel({ id: "c3", name: "Phone", type: "telegram", config: { chat_id: "-1001" } }),
        ],
      },
    });
    renderApp("/channels");
    const list = await screen.findByRole("list", { name: "Channels" });
    const cards = within(list).getAllByRole("listitem");
    expect(cards[0]).toHaveTextContent("Deploy botWebhook · hooks.example.comFailedjust now");
    expect(cards[1]).toHaveTextContent("OpsEmail · ops@x.test, dev@x.testNo delivery yet");
    expect(cards[2]).toHaveTextContent("PhoneTelegram · chat -1001");
  });

  // 001-AC24
  it("has loading, empty and error states", async () => {
    mockApi({ "/api/v1/channels": pending, "/api/v1/deliveries": pending });
    const first = renderApp("/channels");
    expect(screen.getAllByTestId("channel-skeleton")).toHaveLength(2);
    expect(screen.getAllByTestId("delivery-skeleton")).toHaveLength(3);
    first.unmount();
    mockApi();
    const second = renderApp("/channels");
    expect(await screen.findByRole("region", { name: "No channels yet" })).toBeInTheDocument();
    expect(await screen.findByText(/No deliveries yet/)).toBeInTheDocument();
    second.unmount();
    mockApi({
      "/api/v1/channels": () => new Response("", { status: 500 }),
      "/api/v1/deliveries": () => new Response("", { status: 500 }),
    });
    renderApp("/channels");
    expect(await screen.findByText("Channels could not be loaded.")).toBeInTheDocument();
    expect(await screen.findByText("Deliveries could not be loaded.")).toBeInTheDocument();
  });

  // 001-AC16: a generated signing secret is shown once, copyable, and gone once the dialog closes.
  it("creates a webhook and shows its signing secret once", async () => {
    let items: Channel[] = [];
    const fn = mockApi({
      "/api/v1/channels": (_: URL, init?: RequestInit) => {
        if (init?.method !== "POST") return Response.json({ items });
        items = [channel()];
        return Response.json({ ...items[0], signing_secret: GENERATED }, { status: 201 });
      },
    });
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    renderApp("/channels");
    await userEvent.click(
      (await screen.findAllByRole("button", { name: "New channel" }))[0] as HTMLElement,
    );
    const dialog = screen.getByRole("dialog", { name: "New channel" });
    expect(within(dialog).getByRole("radio", { name: "Webhook" })).toBeChecked();
    await userEvent.type(within(dialog).getByLabelText("Name"), "Deploy bot");
    await userEvent.type(within(dialog).getByLabelText("URL"), "https://hooks.example.com/gedour");
    expect(within(dialog).getByLabelText("Signing secret")).toHaveAttribute("type", "password");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create channel" }));
    const box = await within(dialog).findByTestId("signing-secret");
    expect(within(box).getByLabelText("Signing secret")).toHaveValue(GENERATED);
    await userEvent.click(within(box).getByRole("button", { name: "Copy" }));
    expect(writeText).toHaveBeenCalledWith(GENERATED);
    expect(await within(box).findByRole("button", { name: "Copied" })).toBeInTheDocument();
    expect(lastCall(fn, "/api/v1/channels", "POST")?.body).toEqual({
      name: "Deploy bot",
      type: "webhook",
      config: { url: "https://hooks.example.com/gedour" },
    });
    // Escape (or a browser closing it anyway) cannot dismiss the one view of the secret.
    const esc = new Event("cancel", { cancelable: true });
    dialog.dispatchEvent(esc);
    expect(esc.defaultPrevented).toBe(true);
    act(() => (dialog as HTMLDialogElement).close());
    expect(dialog).toHaveAttribute("open");
    expect(within(dialog).getByTestId("signing-secret")).toBeInTheDocument();
    await userEvent.click(within(box).getByRole("button", { name: "I have copied it" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(await screen.findByRole("list", { name: "Channels" })).toHaveTextContent("Deploy bot");
    expect(document.body.innerHTML).not.toContain(GENERATED);

    // Reopened, the form starts over: the secret does not come back.
    await userEvent.click(screen.getByRole("button", { name: "New channel" }));
    expect(screen.queryByTestId("signing-secret")).not.toBeInTheDocument();
  });

  it("closes on Escape when no secret is shown", async () => {
    mockApi();
    renderApp("/channels");
    await userEvent.click(
      (await screen.findAllByRole("button", { name: "New channel" }))[0] as HTMLElement,
    );
    const dialog = screen.getByRole("dialog");
    const esc = new Event("cancel", { cancelable: true });
    dialog.dispatchEvent(esc);
    expect(esc.defaultPrevented).toBe(false);
    act(() => (dialog as HTMLDialogElement).close());
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("creates telegram and email channels with their own fields", async () => {
    const fn = mockApi({
      "/api/v1/channels": (_: URL, init?: RequestInit) =>
        init?.method === "POST"
          ? Response.json(channel({ name: "x", type: "telegram" }), { status: 201 })
          : Response.json({ items: [] }),
    });
    renderApp("/channels");
    await userEvent.click(
      (await screen.findAllByRole("button", { name: "New channel" }))[0] as HTMLElement,
    );
    let dialog = screen.getByRole("dialog");
    await userEvent.click(within(dialog).getByText("Telegram"));
    await userEvent.type(within(dialog).getByLabelText("Name"), "Phone");
    await userEvent.type(within(dialog).getByLabelText("Chat ID"), "-1001");
    await userEvent.type(within(dialog).getByLabelText("Bot token"), "123:abc");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create channel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(lastCall(fn, "/api/v1/channels", "POST")?.body).toEqual({
      name: "Phone",
      type: "telegram",
      config: { chat_id: "-1001", silent: false },
      secret: "123:abc",
    });

    await userEvent.click(screen.getAllByRole("button", { name: "New channel" })[0] as HTMLElement);
    dialog = screen.getByRole("dialog");
    await userEvent.click(within(dialog).getByText("Email"));
    expect(within(dialog).queryByLabelText(/secret|token/i)).not.toBeInTheDocument();
    await userEvent.type(within(dialog).getByLabelText("Name"), "Ops");
    await userEvent.type(
      within(dialog).getByLabelText("Recipients"),
      "a@x.test, b@x.test{enter}c@x.test",
    );
    await userEvent.click(within(dialog).getByRole("button", { name: "Create channel" }));
    await waitFor(() =>
      expect(lastCall(fn, "/api/v1/channels", "POST")?.body).toEqual({
        name: "Ops",
        type: "email",
        config: { to: ["a@x.test", "b@x.test", "c@x.test"] },
      }),
    );
  });

  // 003-AC3: the copy tells a refusal apart from a failure that retries; a dead delivery can be retried.
  it("says a refused send is not retried but can be retried by hand", async () => {
    mockApi({
      "/api/v1/channels": { items: [channel({ type: "nextcloud_talk" })] },
      "/api/v1/deliveries": {
        items: [delivery(3, { status: "dead", last_error: "POST cloud.test: HTTP 401" })],
      },
    });
    renderApp("/channels");
    expect(
      await screen.findByText(
        "Failed sends are tried up to 5 times with growing delays. A refusal that cannot succeed is not retried, but you can retry it by hand.",
      ),
    ).toBeInTheDocument();
    const table = await screen.findByRole("table", { name: "Deliveries" });
    expect(table).toHaveTextContent("HTTP 401");
    expect(within(table).getByRole("button", { name: "Retry" })).toBeInTheDocument();
  });

  // 003-AC1: the Talk fields, the setup commands and a write-only secret.
  it("creates a Nextcloud Talk channel", async () => {
    const fn = mockApi({
      "/api/v1/channels": (_: URL, init?: RequestInit) =>
        init?.method === "POST"
          ? Response.json(channel({ name: "Room", type: "nextcloud_talk" }), { status: 201 })
          : Response.json({ items: [] }),
    });
    renderApp("/channels");
    await userEvent.click(
      (await screen.findAllByRole("button", { name: "New channel" }))[0] as HTMLElement,
    );
    const dialog = screen.getByRole("dialog");
    await userEvent.click(within(dialog).getByText("Nextcloud Talk"));
    expect(dialog).toHaveTextContent("occ talk:bot:create -s <secret> gedour");
    expect(dialog).toHaveTextContent(
      "occ talk:bot:install --feature response <name> <secret> <url>",
    );
    expect(dialog).toHaveTextContent("occ talk:bot:setup <bot-id> <token>");
    await userEvent.type(within(dialog).getByLabelText("Name"), "Room");
    await userEvent.type(within(dialog).getByLabelText("Nextcloud URL"), "https://cloud.test/");
    await userEvent.type(within(dialog).getByLabelText("Conversation token"), "abcd1234");
    const secret = within(dialog).getByLabelText("Bot secret");
    expect(secret).toHaveAttribute("type", "password");
    await userEvent.type(secret, "s".repeat(40));
    await userEvent.click(within(dialog).getByRole("button", { name: "Create channel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(lastCall(fn, "/api/v1/channels", "POST")?.body).toEqual({
      name: "Room",
      type: "nextcloud_talk",
      config: { url: "https://cloud.test/", conversation: "abcd1234", silent: false },
      secret: "s".repeat(40),
    });
  });

  // 003-AC1: editing keeps the secret unless a new one is typed; server field errors land on their field.
  it("edits a Nextcloud Talk channel without showing its secret", async () => {
    let refuse = true;
    const talk = channel({
      type: "nextcloud_talk",
      config: { url: "https://cloud.test", conversation: "abcd1234", silent: true },
    });
    const fn = mockApi({
      "/api/v1/channels": { items: [talk] },
      "/api/v1/channels/c1": () =>
        refuse
          ? problem(400, "invalid channel", [
              { location: "body.config.conversation", message: "the conversation token" },
            ])
          : Response.json(talk),
    });
    renderApp("/channels");
    await userEvent.click(await screen.findByRole("button", { name: "Edit Deploy bot" }));
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByLabelText("Bot secret")).toHaveValue("");
    expect(within(dialog).getByLabelText("Bot secret")).toHaveAttribute("placeholder", "••••abcd");
    expect(within(dialog).getByRole("switch", { name: "Silent" })).toBeChecked();
    await userEvent.click(within(dialog).getByRole("button", { name: "Save channel" }));
    expect(await within(dialog).findAllByText(/the conversation token/)).toHaveLength(1);
    refuse = false;
    await userEvent.click(within(dialog).getByRole("switch", { name: "Silent" }));
    await userEvent.click(within(dialog).getByRole("button", { name: "Save channel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(lastCall(fn, "/api/v1/channels/c1", "PATCH")?.body).toEqual({
      name: "Deploy bot",
      config: { url: "https://cloud.test", conversation: "abcd1234", silent: false },
    });
  });

  // 003-AC5: the switch is on the Telegram and Talk forms only, named and operable from the keyboard.
  it("shows Silent on Telegram and Nextcloud Talk only", async () => {
    mockApi();
    renderApp("/channels");
    await userEvent.click(
      (await screen.findAllByRole("button", { name: "New channel" }))[0] as HTMLElement,
    );
    const dialog = screen.getByRole("dialog");
    const silent = () => within(dialog).queryByRole("switch", { name: "Silent" });
    for (const kind of ["Email", "Webhook", "Script"]) {
      await userEvent.click(within(dialog).getByText(kind));
      expect(silent()).not.toBeInTheDocument();
    }
    for (const [kind, hint] of [
      ["Telegram", "Notifies without sound."],
      ["Nextcloud Talk", "Posts without a notification."],
    ] as const) {
      await userEvent.click(within(dialog).getByText(kind));
      expect(silent()).toHaveAccessibleDescription(hint);
      expect(silent()).not.toBeChecked();
    }
    (silent() as HTMLElement).focus();
    await userEvent.keyboard(" ");
    expect(silent()).toBeChecked();
    await userEvent.keyboard("{Enter}");
    expect(silent()).not.toBeChecked();
  });

  // Email without an SMTP server: the type is disabled with the server's hint.
  it("disables email when the instance has no SMTP", async () => {
    mockApi({
      "/api/v1/channel-types": {
        items: CHANNEL_TYPES.map((t) =>
          t.type === "email" ? { ...t, available: false, hint: "email needs an SMTP server" } : t,
        ),
      },
    });
    renderApp("/channels");
    await userEvent.click(
      (await screen.findAllByRole("button", { name: "New channel" }))[0] as HTMLElement,
    );
    const dialog = screen.getByRole("dialog");
    expect(await within(dialog).findByRole("radio", { name: "Email" })).toBeDisabled();
    expect(within(dialog).getByText("email needs an SMTP server")).toBeInTheDocument();
  });

  it("shows field errors and edits a channel without resending its secret", async () => {
    const fn = mockApi({
      "/api/v1/channels": { items: [channel()] },
      "/api/v1/channels/c1": (_: URL, init?: RequestInit) => {
        const body = JSON.parse(String(init?.body)) as { config: { url: string } };
        return body.config.url.startsWith("http://")
          ? problem(400, "invalid channel", [
              { location: "body.config.url", message: "must use https" },
              { location: "body.config", message: "unknown field" },
            ])
          : Response.json(channel({ name: "Renamed" }));
      },
    });
    renderApp("/channels");
    await userEvent.click(await screen.findByRole("button", { name: "Edit Deploy bot" }));
    const dialog = screen.getByRole("dialog", { name: "Edit Deploy bot" });
    expect(within(dialog).queryByRole("radio")).not.toBeInTheDocument();
    expect(within(dialog).getByLabelText("Signing secret")).toHaveAttribute(
      "placeholder",
      "••••abcd",
    );
    const url = within(dialog).getByLabelText("URL");
    await userEvent.clear(url);
    await userEvent.type(url, "http://hooks.example.com/x");
    await userEvent.click(within(dialog).getByRole("button", { name: "Save channel" }));
    expect(await within(dialog).findByText("must use https")).toBeInTheDocument();
    expect(within(dialog).getByRole("alert")).toHaveTextContent("config: unknown field");
    await userEvent.clear(url);
    await userEvent.type(url, "https://hooks.example.com/x");
    await userEvent.click(within(dialog).getByRole("button", { name: "Save channel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(lastCall(fn, "/api/v1/channels/c1", "PATCH")?.body).toEqual({
      name: "Deploy bot",
      config: { url: "https://hooks.example.com/x" },
    });
  });

  // 001-AC16: "Send test" reports the outcome in a toast.
  it("sends a test and tells the result", async () => {
    let answer: Response = Response.json({ ok: true });
    mockApi({
      "/api/v1/channels": { items: [channel()] },
      "/api/v1/channels/c1/test": () => answer,
    });
    renderApp("/channels");
    await userEvent.click(await screen.findByRole("button", { name: "Send test" }));
    expect(await screen.findByText("Test message sent to Deploy bot")).toBeInTheDocument();
    answer = Response.json({ ok: false, error: "POST hooks.example.com: HTTP 500" });
    await userEvent.click(screen.getByRole("button", { name: "Send test" }));
    expect(
      await screen.findByText("Test to Deploy bot failed: POST hooks.example.com: HTTP 500"),
    ).toBeInTheDocument();
    answer = problem(429, "a test was sent less than 10s ago: retry in 7 s");
    await userEvent.click(screen.getByRole("button", { name: "Send test" }));
    expect(
      await screen.findByText("a test was sent less than 10s ago: retry in 7 s"),
    ).toBeInTheDocument();
  });

  it("names the rules that block a delete", async () => {
    let fail = true;
    const fn = mockApi({
      "/api/v1/channels": { items: [channel()] },
      "/api/v1/channels/c1": () =>
        fail
          ? problem(409, "used by Releases", [
              { location: "rules", message: "sends to this channel", value: "Releases" },
            ])
          : new Response(null, { status: 204 }),
    });
    renderApp("/channels");
    await userEvent.click(await screen.findByRole("button", { name: "Delete Deploy bot" }));
    let dialog = screen.getByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent(
      "These rules still send to it:Releases",
    );
    await userEvent.click(within(dialog).getByRole("button", { name: "Close" }));
    fail = false;
    await userEvent.click(screen.getByRole("button", { name: "Delete Deploy bot" }));
    dialog = screen.getByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(lastCall(fn, "/api/v1/channels/c1", "DELETE")).toBeDefined();
  });

  it("shows other delete errors", async () => {
    mockApi({
      "/api/v1/channels": { items: [channel()] },
      "/api/v1/channels/c1": () => problem(500, "database down"),
    });
    renderApp("/channels");
    await userEvent.click(await screen.findByRole("button", { name: "Delete Deploy bot" }));
    await userEvent.click(
      within(await screen.findByRole("dialog")).getByRole("button", { name: "Delete" }),
    );
    expect(
      await within(await screen.findByRole("dialog")).findByText("database down"),
    ).toBeInTheDocument();
  });

  // 001-AC15: the deliveries table shows status, attempts, next attempt and the last error, with a retry.
  it("lists deliveries, filters them and retries a failed one", async () => {
    const fn = mockApi({
      "/api/v1/channels": { items: [channel()] },
      "/api/v1/deliveries": (url: URL) =>
        Response.json({
          items: url.searchParams.get("cursor")
            ? [delivery(1)]
            : [
                delivery(3, {
                  status: "dead",
                  attempts: 5,
                  last_error: "POST hooks.example.com: HTTP 502\nupstream down",
                  sent_at: null,
                }),
                delivery(2, {
                  status: "failed",
                  attempts: 2,
                  next_attempt_at: new Date(Date.now() + 120_000).toISOString(),
                  rule: undefined,
                }),
              ],
          next_cursor: url.searchParams.get("cursor") ? undefined : "2",
        }),
      "/api/v1/deliveries/3/retry": Response.json(delivery(3, { status: "pending" })),
    });
    renderApp("/channels");
    const table = await screen.findByRole("table", { name: "Deliveries" });
    await within(table).findByText("POST hooks.example.com: HTTP 502");
    const rows = within(table).getAllByRole("row");
    expect(rows[1]).toHaveTextContent("Failedv2.104.0 published");
    expect(rows[1]).toHaveTextContent("5 / 5");
    expect(rows[2]).toHaveTextContent("Retrying");
    expect(rows[2]).toHaveTextContent("in 2 minutes");
    expect(
      within(rows[1] as HTMLElement).getByText("POST hooks.example.com: HTTP 502"),
    ).toBeInTheDocument();
    expect(within(table).getAllByRole("button", { name: "Retry" })).toHaveLength(2);
    await userEvent.click(within(rows[1] as HTMLElement).getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("Delivery queued again")).toBeInTheDocument();
    expect(lastCall(fn, "/api/v1/deliveries/3/retry", "POST")).toBeDefined();

    await userEvent.click(screen.getByRole("button", { name: "Load more" }));
    expect(await within(table).findAllByRole("row")).toHaveLength(4);

    await userEvent.selectOptions(screen.getByLabelText("Channel"), "c1");
    await userEvent.selectOptions(screen.getByLabelText("Status"), "dead");
    await waitFor(() => {
      const q = lastCall(fn, "/api/v1/deliveries")?.url.searchParams;
      expect(q?.get("channel_id")).toBe("c1");
      expect(q?.get("status")).toBe("dead");
    });
  });

  it("says when nothing matches the filters or a retry fails", async () => {
    mockApi({
      "/api/v1/deliveries": (url: URL) =>
        Response.json({
          items: url.searchParams.get("status") ? [] : [delivery(3, { status: "dead" })],
        }),
      "/api/v1/deliveries/3/retry": () => problem(409, "this one is pending"),
    });
    renderApp("/channels");
    await userEvent.click(await screen.findByRole("button", { name: "Retry" }));
    expect(await screen.findByText("Could not retry: this one is pending")).toBeInTheDocument();
    await userEvent.selectOptions(screen.getByLabelText("Status"), "sent");
    expect(await screen.findByText("No delivery matches these filters.")).toBeInTheDocument();
  });

  // 001-AC17
  it("creates a script channel from one of the user's scripts", async () => {
    const fn = mockApi({
      "/api/v1/scripts": { items: [script(), script({ id: "s2", name: "Other" })] },
      "/api/v1/channels": (_: URL, init?: RequestInit) =>
        init?.method === "POST"
          ? Response.json(
              channel({ name: "Run it", type: "script", config: { script_id: "s2" } }),
              {
                status: 201,
              },
            )
          : Response.json({
              items: [channel({ name: "Runs", type: "script", config: { script_id: "s1" } })],
            }),
    });
    renderApp("/channels");
    expect(await screen.findByRole("list", { name: "Channels" })).toHaveTextContent(
      "RunsScript · runs Notify deploy",
    );
    await userEvent.click(screen.getByRole("button", { name: "New channel" }));
    const dialog = screen.getByRole("dialog");
    await userEvent.click(within(dialog).getByRole("radio", { name: "Script" }));
    expect(within(dialog).queryByLabelText(/secret|token/i)).not.toBeInTheDocument();
    await userEvent.type(within(dialog).getByLabelText("Name"), "Run it");
    await userEvent.selectOptions(
      within(dialog).getByRole("combobox", { name: "Script" }),
      "Other",
    );
    await userEvent.click(within(dialog).getByRole("button", { name: "Create channel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(lastCall(fn, "/api/v1/channels", "POST")?.body).toEqual({
      name: "Run it",
      type: "script",
      config: { script_id: "s2" },
    });
  });

  it("points to the script editor when there is no script yet", async () => {
    mockApi({ "/api/v1/scripts": { items: [] } });
    renderApp("/channels");
    await userEvent.click(
      (await screen.findAllByRole("button", { name: "New channel" }))[0] as HTMLElement,
    );
    const dialog = screen.getByRole("dialog");
    await userEvent.click(within(dialog).getByRole("radio", { name: "Script" }));
    expect(await within(dialog).findByRole("link", { name: "Write one" })).toHaveAttribute(
      "href",
      "/scripts/new",
    );
  });

  // 001-AC17: a script delivery shows its run's logs, folded.
  it("shows the logs of script deliveries", async () => {
    mockApi({
      "/api/v1/deliveries": {
        items: [
          delivery(1, {
            channel: { id: "c9", name: "Runs", type: "script" },
            logs: "received release.published\nreceiver answered 200",
          }),
          delivery(2, {
            status: "failed",
            channel: { id: "c9", name: "Runs", type: "script" },
            logs: "",
            last_error: "script error: Error: boom",
          }),
          delivery(3),
        ],
      },
    });
    renderApp("/channels");
    const table = await screen.findByRole("table", { name: "Deliveries" });
    const logs = await within(table).findByText("Logs · 2 lines");
    const rows = within(table).getAllByRole("row");
    await userEvent.click(logs);
    expect(within(rows[1] as HTMLElement).getByText(/receiver answered 200/)).toBeVisible();
    expect(rows[2]).toHaveTextContent("Logs · 0 lines");
    expect(rows[2]).toHaveTextContent("No log line.");
    expect(rows[2]).toHaveTextContent("script error: Error: boom");
    expect(rows[3]).not.toHaveTextContent("Logs");
  });

  // 001-AC17: a script channel's test is a live run; its logs and error show under the card.
  it("shows the logs of a script channel's test run", async () => {
    let n = 0;
    mockApi({
      "/api/v1/channels": {
        items: [channel({ name: "Runs", type: "script", config: { script_id: "s1" } })],
      },
      "/api/v1/channels/c1/test": () =>
        ++n === 1
          ? Response.json({ ok: true, logs: ["posted", "receiver 200"] })
          : Response.json({ ok: false, error: "script error: Error: nope", logs: [] }),
    });
    renderApp("/channels");
    const card = (await screen.findByRole("list", { name: "Channels" })).querySelector("li");
    const run = within(card as HTMLElement).getByRole("button", { name: "Run test" });
    await userEvent.click(run);
    const panel = await screen.findByRole("region", { name: "Test run of Runs" });
    expect(panel).toHaveTextContent("Live run returned");
    expect(panel).toHaveTextContent("its http.fetch calls were sent");
    expect(panel).toHaveTextContent("posted receiver 200");
    expect(await screen.findByText("Test run of Runs done")).toBeInTheDocument();
    await userEvent.click(run);
    await waitFor(() =>
      expect(panel).toHaveTextContent("Live run failed: script error: Error: nope"),
    );
    expect(panel).toHaveTextContent("No log line.");
    await userEvent.click(within(panel).getByRole("button", { name: "Hide" }));
    expect(screen.queryByRole("region", { name: "Test run of Runs" })).not.toBeInTheDocument();
  });

  // 012-AC11: Delivery is offered on the four digest types only; Digest shows its times and zone.
  it("creates a digest channel with its times and time zone, checked before sending", async () => {
    const fn = mockApi({
      "/api/v1/channels": (_: URL, init?: RequestInit) =>
        init?.method === "POST"
          ? Response.json(channel({ name: "Phone", type: "telegram" }), { status: 201 })
          : Response.json({ items: [] }),
    });
    renderApp("/channels");
    await userEvent.click(
      (await screen.findAllByRole("button", { name: "New channel" }))[0] as HTMLElement,
    );
    const dialog = screen.getByRole("dialog");
    const digest = () => within(dialog).queryByRole("radio", { name: /^Digest/ });
    for (const kind of ["Webhook", "Script"]) {
      await userEvent.click(within(dialog).getByText(kind));
      expect(digest()).not.toBeInTheDocument();
    }
    for (const kind of ["Email", "Web Push", "Nextcloud Talk", "Telegram"]) {
      await userEvent.click(within(dialog).getByText(kind));
      expect(within(dialog).getByRole("radio", { name: /^Immediate/ })).toBeChecked();
    }
    expect(within(dialog).queryByRole("list", { name: "Digest times" })).not.toBeInTheDocument();
    await userEvent.type(within(dialog).getByLabelText("Name"), "Phone");
    await userEvent.type(within(dialog).getByLabelText("Chat ID"), "-1001");
    await userEvent.type(within(dialog).getByLabelText("Bot token"), "123:abc");
    await userEvent.click(digest() as HTMLElement);
    expect(within(dialog).getByLabelText("Time 1")).toHaveValue("09:00");
    expect(within(dialog).getByRole("button", { name: "Remove time 1" })).toBeDisabled();
    const zone = within(dialog).getByLabelText("Time zone");
    expect(zone).toHaveValue(Intl.DateTimeFormat().resolvedOptions().timeZone);
    expect(zone).toHaveAccessibleDescription("The times are local to this zone.");

    // A duplicate, an empty row and an unknown zone are refused here, as the server would.
    await userEvent.click(within(dialog).getByRole("button", { name: "Add a time" }));
    await userEvent.click(within(dialog).getByRole("button", { name: "Add a time" }));
    await userEvent.type(within(dialog).getByLabelText("Time 2"), "09:00");
    await userEvent.clear(zone);
    await userEvent.type(zone, "Mars/Olympus");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create channel" }));
    expect(within(dialog).getByText("09:00 is already listed")).toBeInTheDocument();
    expect(within(dialog).getByLabelText("Time 2")).toHaveAttribute("aria-invalid", "true");
    expect(within(dialog).getByLabelText("Time 3")).toHaveAttribute("aria-invalid", "true");
    // Each row is described by its own error only.
    expect(within(dialog).getByLabelText("Time 1")).not.toHaveAttribute("aria-describedby");
    expect(within(dialog).getByLabelText("Time 2")).toHaveAccessibleDescription(
      "09:00 is already listed",
    );
    expect(within(dialog).getByLabelText("Time 3")).toHaveAccessibleDescription(
      "a local time as HH:MM, e.g. 09:00",
    );
    expect(within(dialog).getByText("an IANA time zone, e.g. Europe/Paris")).toBeInTheDocument();
    expect(lastCall(fn, "/api/v1/channels", "POST")).toBeUndefined();

    await userEvent.click(within(dialog).getByRole("button", { name: "Remove time 3" }));
    // The rows changed: their errors go with them.
    expect(within(dialog).queryByText("09:00 is already listed")).not.toBeInTheDocument();
    expect(within(dialog).getByLabelText("Time 2")).not.toHaveAttribute("aria-invalid");
    const second = within(dialog).getByLabelText("Time 2");
    await userEvent.clear(second);
    await userEvent.type(second, "18:00");
    await userEvent.clear(zone);
    await userEvent.type(zone, "Europe/Paris");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create channel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(lastCall(fn, "/api/v1/channels", "POST")?.body).toEqual({
      name: "Phone",
      type: "telegram",
      config: {
        chat_id: "-1001",
        silent: false,
        delivery: "digest",
        at: ["09:00", "18:00"],
        tz: "Europe/Paris",
      },
      secret: "123:abc",
    });
  });

  // 010-AC5, 012-AC11: a Web Push channel with picked devices and a digest sends both parts of its config.
  it("sends picked devices and a digest for Web Push", async () => {
    const fn = mockApi({
      "/api/v1/push/devices": {
        items: [
          {
            id: "d1",
            name: "Work laptop",
            user_agent: "Firefox/140",
            endpoint_sha256: "00",
            created_at: "2026-11-01T00:00:00Z",
            last_push_at: null,
          },
        ],
      },
      "/api/v1/channels": (_: URL, init?: RequestInit) =>
        init?.method === "POST"
          ? Response.json(channel({ type: "web_push", config: { devices: ["d1"] } }), {
              status: 201,
            })
          : Response.json({ items: [] }),
    });
    renderApp("/channels");
    await userEvent.click(
      (await screen.findAllByRole("button", { name: "New channel" }))[0] as HTMLElement,
    );
    const dialog = screen.getByRole("dialog", { name: "New channel" });
    await userEvent.click(within(dialog).getByText("Web Push"));
    await userEvent.click(within(dialog).getByLabelText(/Pick devices/));
    await userEvent.click(await within(dialog).findByRole("checkbox", { name: "Work laptop" }));
    await userEvent.click(within(dialog).getByRole("radio", { name: /^Digest/ }));
    await userEvent.type(within(dialog).getByLabelText("Name"), "Pushes");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create channel" }));
    await waitFor(() =>
      expect(lastCall(fn, "/api/v1/channels", "POST")?.body).toEqual({
        name: "Pushes",
        type: "web_push",
        config: {
          devices: ["d1"],
          silent: false,
          delivery: "digest",
          at: ["09:00"],
          tz: Intl.DateTimeFormat().resolvedOptions().timeZone,
        },
      }),
    );
  });

  // 012-AC11: an empty zone is sent as UTC, 24 rows is the cap, and a server error lands on its row.
  it("caps the digest times and places the server's schedule errors", async () => {
    const fn = mockApi({
      "/api/v1/channels": (_: URL, init?: RequestInit) =>
        init?.method === "POST"
          ? problem(400, "invalid channel", [
              { location: "body.config.at[0]", message: "server says no" },
              { location: "body.config.delivery", message: "digest refused" },
            ])
          : Response.json({ items: [] }),
    });
    renderApp("/channels");
    await userEvent.click(
      (await screen.findAllByRole("button", { name: "New channel" }))[0] as HTMLElement,
    );
    const dialog = screen.getByRole("dialog");
    await userEvent.click(within(dialog).getByText("Email"));
    await userEvent.click(within(dialog).getByRole("radio", { name: /^Digest/ }));
    await userEvent.clear(within(dialog).getByLabelText("Time zone"));
    const add = within(dialog).getByRole("button", { name: "Add a time" });
    for (let i = 1; i < 24; i++) await userEvent.click(add);
    expect(add).toBeDisabled();
    for (let i = 24; i > 1; i--)
      await userEvent.click(within(dialog).getByRole("button", { name: `Remove time ${i}` }));
    await userEvent.click(within(dialog).getByRole("button", { name: "Create channel" }));
    expect(await within(dialog).findByText("server says no")).toBeInTheDocument();
    expect(within(dialog).getByText("digest refused")).toBeInTheDocument();
    expect(within(dialog).queryByRole("alert")).not.toBeInTheDocument();
    for (const radio of ["Immediate", "Digest"])
      expect(
        within(dialog).getByRole("radio", { name: new RegExp(`^${radio}`) }),
      ).toHaveAccessibleDescription("digest refused");
    expect(within(dialog).getByLabelText("Time 1")).toHaveAccessibleDescription("server says no");
    await userEvent.click(within(dialog).getByRole("button", { name: "Add a time" }));
    expect(within(dialog).queryByText("server says no")).not.toBeInTheDocument();
    expect(lastCall(fn, "/api/v1/channels", "POST")?.body).toMatchObject({
      config: { delivery: "digest", at: ["09:00"], tz: "UTC" },
    });
  });

  // 012-AC11: the card names the schedule; editing shows it back, and Immediate drops its keys.
  it("shows and edits a digest channel's schedule", async () => {
    const tg = channel({
      type: "telegram",
      config: {
        chat_id: "-1001",
        silent: true,
        delivery: "digest",
        at: ["09:00", "18:00"],
        tz: "Europe/Paris",
      },
    });
    const fn = mockApi({
      "/api/v1/channels": { items: [tg] },
      "/api/v1/channels/c1": Response.json(tg),
    });
    renderApp("/channels");
    const list = await screen.findByRole("list", { name: "Channels" });
    expect(within(list).getByText("Digest at 09:00, 18:00 (Europe/Paris)")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Edit Deploy bot" }));
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByRole("radio", { name: /^Digest/ })).toBeChecked();
    expect(within(dialog).getByLabelText("Time 1")).toHaveValue("09:00");
    expect(within(dialog).getByLabelText("Time 2")).toHaveValue("18:00");
    expect(within(dialog).getByLabelText("Time zone")).toHaveValue("Europe/Paris");
    await userEvent.click(within(dialog).getByRole("button", { name: "Save channel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(lastCall(fn, "/api/v1/channels/c1", "PATCH")?.body).toEqual({
      name: "Deploy bot",
      config: tg.config,
    });

    await userEvent.click(screen.getByRole("button", { name: "Edit Deploy bot" }));
    const again = screen.getByRole("dialog");
    await userEvent.click(within(again).getByRole("radio", { name: /^Immediate/ }));
    expect(within(again).queryByLabelText("Time zone")).not.toBeInTheDocument();
    await userEvent.click(within(again).getByRole("button", { name: "Save channel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(lastCall(fn, "/api/v1/channels/c1", "PATCH")?.body).toEqual({
      name: "Deploy bot",
      config: { chat_id: "-1001", silent: true },
    });
  });

  // 012-AC11: a skipped delivery shows its reason, not as an error, and has no retry.
  it("shows a skipped delivery with its reason", async () => {
    const fn = mockApi({
      "/api/v1/channels": { items: [channel()] },
      "/api/v1/deliveries": {
        items: [
          delivery(4, {
            status: "skipped",
            last_error: "not sent: read in the inbox before the digest",
          }),
        ],
      },
    });
    renderApp("/channels");
    const table = await screen.findByRole("table", { name: "Deliveries" });
    const reason = await within(table).findByText("not sent: read in the inbox before the digest", {
      selector: "summary",
    });
    expect(reason).toHaveClass("text-muted");
    expect(reason).not.toHaveClass("text-rose-700");
    expect(within(table).getAllByRole("row")[1]).toHaveTextContent("Skipped");
    expect(within(table).queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
    expect(
      within(screen.getByLabelText("Status"))
        .getAllByRole("option")
        .map((o) => o.textContent),
    ).toEqual(["Any status", "Pending", "Sent", "Retrying", "Failed", "Skipped"]);
    await userEvent.selectOptions(screen.getByLabelText("Status"), "skipped");
    await waitFor(() =>
      expect(lastCall(fn, "/api/v1/deliveries")?.url.searchParams.get("status")).toBe("skipped"),
    );
  });

  // 012-AC11: focus follows the rows: a new row takes it, a removed one hands it back.
  it("moves focus as digest times are added and removed", async () => {
    mockApi();
    renderApp("/channels");
    await userEvent.click(
      (await screen.findAllByRole("button", { name: "New channel" }))[0] as HTMLElement,
    );
    const dialog = screen.getByRole("dialog");
    await userEvent.click(within(dialog).getByText("Telegram"));
    await userEvent.click(within(dialog).getByRole("radio", { name: /^Digest/ }));
    const add = within(dialog).getByRole("button", { name: "Add a time" });
    await userEvent.click(add);
    await userEvent.click(add);
    expect(document.activeElement).toBe(within(dialog).getByLabelText("Time 3"));
    await userEvent.click(within(dialog).getByRole("button", { name: "Remove time 3" }));
    expect(document.activeElement).toBe(within(dialog).getByLabelText("Time 2"));
    await userEvent.click(within(dialog).getByRole("button", { name: "Remove time 1" }));
    expect(document.activeElement).toBe(add);
    expect(within(dialog).getAllByRole("listitem")).toHaveLength(1);
  });

  // 012-AC11: Digest picked on a digest type, then a type without it: nothing of the schedule is sent.
  it("drops the schedule when the type has no digest", async () => {
    const fn = mockApi({
      "/api/v1/channels": (_: URL, init?: RequestInit) =>
        init?.method === "POST"
          ? Response.json(channel(), { status: 201 })
          : Response.json({ items: [] }),
    });
    renderApp("/channels");
    await userEvent.click(
      (await screen.findAllByRole("button", { name: "New channel" }))[0] as HTMLElement,
    );
    const dialog = screen.getByRole("dialog");
    await userEvent.click(within(dialog).getByText("Telegram"));
    await userEvent.click(within(dialog).getByRole("radio", { name: /^Digest/ }));
    await userEvent.click(within(dialog).getByText("Webhook"));
    await userEvent.type(within(dialog).getByLabelText("Name"), "Hook");
    await userEvent.type(within(dialog).getByLabelText("URL"), "https://hooks.example.com/x");
    await userEvent.type(within(dialog).getByLabelText("Signing secret"), "s3cret");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create channel" }));
    await waitFor(() =>
      expect(lastCall(fn, "/api/v1/channels", "POST")?.body).toEqual({
        name: "Hook",
        type: "webhook",
        config: { url: "https://hooks.example.com/x" },
        secret: "s3cret",
      }),
    );
  });
});

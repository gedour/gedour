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
});

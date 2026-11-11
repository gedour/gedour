import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { mockApi } from "@/test/api";
import { renderApp } from "@/test/render";
import { channel, preview, rule } from "@/test/routing";
import { lastCall, pending, problem, watch } from "@/test/watches";
import type { Rule } from "@/types/api";

const filterProblem = (line: number, column: number, message = "unexpected token EOF") =>
  problem(400, "invalid filter", [
    {
      location: "body.filter",
      message: `${message} (line ${line}, column ${column})`,
      value: { line, column },
    },
  ]);

describe("RulesPage", () => {
  it("lists rules with their scope, filter and channels, and toggles them", async () => {
    let rules: Rule[] = [
      rule(),
      rule({
        id: "r2",
        name: "Tags",
        watch_ids: ["w1"],
        watches: [{ id: "w1", name: "Postgres tags" }],
        filter: "",
        channel_ids: [],
        enabled: false,
      }),
      rule({ id: "r3", name: "Lost", watch_ids: ["w9"], watches: [{ id: "w9", name: null }] }),
    ];
    const fn = mockApi({
      "/api/v1/rules": { items: rules },
      "/api/v1/channels": { items: [channel()] },
      "/api/v1/rules/r2": (_: URL, init?: RequestInit) => {
        const body = JSON.parse(String(init?.body)) as { enabled: boolean };
        rules = [rules[0] as Rule, { ...(rules[1] as Rule), enabled: body.enabled }];
        return Response.json(rules[1]);
      },
    });
    renderApp("/rules");
    const list = await screen.findByRole("list", { name: "Rules" });
    const items = within(list)
      .getAllByRole("listitem")
      .filter((li) => li.parentElement === list);
    expect(items[0]).toHaveTextContent('All watches·event.type == "release.published"');
    expect(
      await within(items[0] as HTMLElement).findByRole("list", { name: "Channels of Releases" }),
    ).toHaveTextContent("InboxDeploy bot");
    expect(items[1]).toHaveTextContent("Postgres tags·every event");
    expect(items[2]).toHaveTextContent("A watch you no longer own");
    const sw = screen.getByRole("switch", { name: "Tags enabled" });
    expect(sw).toHaveAttribute("aria-checked", "false");
    await userEvent.click(sw);
    expect(lastCall(fn, "/api/v1/rules/r2", "PATCH")?.body).toEqual({ enabled: true });
    expect(await screen.findByText("Tags enabled")).toBeInTheDocument();
  });

  // 014-AC19: all watches, the one name, two names, or the first two and a "+N" button opening every name.
  it("names the watches a rule covers", async () => {
    mockApi({
      "/api/v1/rules": {
        items: [
          rule({ id: "r1", name: "All", watch_ids: null, watches: null }),
          rule({ id: "r2", name: "One", watch_ids: ["w0"], watches: [{ id: "w0", name: "Go" }] }),
          rule({
            id: "r3",
            name: "Two",
            watch_ids: ["w0", "w9"],
            watches: [
              { id: "w0", name: "Go" },
              { id: "w9", name: null },
            ],
          }),
          rule({
            id: "r4",
            name: "Four",
            watch_ids: ["w0", "w1", "w2", "w3"],
            // In id order: the label sorts them by name, the lost one last.
            watches: ["Redis", null, "Postgres", "Go"].map((name, i) => ({ id: `w${i}`, name })),
          }),
        ],
      },
    });
    renderApp("/rules");
    const list = await screen.findByRole("list", { name: "Rules" });
    const items = within(list)
      .getAllByRole("listitem")
      .filter((li) => li.parentElement === list);
    expect(items.map((li) => li.querySelector("p")?.firstElementChild?.textContent)).toEqual([
      "All watches",
      "Go",
      "Go, A watch you no longer own",
      "Go, Postgres+2",
    ]);
    // "+2" opens every name, focus in the list; Escape closes it and hands focus back.
    const more = within(items[3] as HTMLElement).getByRole("button", {
      name: "Show all 4 watches",
    });
    expect(more).toHaveTextContent("+2");
    await userEvent.click(more);
    const dialog = await screen.findByRole("dialog", { name: "Watches of Four" });
    expect(
      within(dialog)
        .getAllByRole("listitem")
        .map((li) => li.textContent),
    ).toEqual(["Go", "Postgres", "Redis", "A watch you no longer own"]);
    expect(within(dialog).getByRole("list")).toHaveFocus();
    expect(within(dialog).getByRole("list")).toHaveClass("overflow-y-auto");
    await userEvent.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(more).toHaveFocus();
    // An outside click closes it too.
    await userEvent.click(more);
    await screen.findByRole("dialog", { name: "Watches of Four" });
    await userEvent.click(list);
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });

  // 005-AC7: a high rule says so in the list, by text; a normal one shows nothing.
  it("marks high rules in the list", async () => {
    mockApi({
      "/api/v1/rules": {
        items: [rule(), rule({ id: "r2", name: "Urgent", priority: "high" })],
      },
      "/api/v1/channels": { items: [channel()] },
    });
    renderApp("/rules");
    const list = await screen.findByRole("list", { name: "Rules" });
    const items = within(list)
      .getAllByRole("listitem")
      .filter((li) => li.parentElement === list);
    expect(items[0]).not.toHaveTextContent("High priority");
    expect(items[1]).toHaveTextContent("High priority");
  });

  // 001-AC24
  it("has loading, empty and error states", async () => {
    mockApi({ "/api/v1/rules": pending });
    const first = renderApp("/rules");
    expect(screen.getAllByTestId("rule-skeleton")).toHaveLength(3);
    first.unmount();
    mockApi();
    const second = renderApp("/rules");
    expect(await screen.findByRole("region", { name: "No rules yet" })).toBeInTheDocument();
    second.unmount();
    let calls = 0;
    mockApi({
      "/api/v1/rules": () =>
        calls++ ? Response.json({ items: [rule()] }) : new Response("", { status: 500 }),
    });
    renderApp("/rules");
    expect(await screen.findByRole("alert")).toHaveTextContent("Rules could not be loaded.");
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("link", { name: "Releases" })).toBeInTheDocument();
  });

  it("says when a toggle fails", async () => {
    mockApi({
      "/api/v1/rules": { items: [rule()] },
      "/api/v1/rules/r1": () => problem(500, "boom"),
    });
    renderApp("/rules");
    await userEvent.click(await screen.findByRole("switch"));
    expect(await screen.findByText(/Could not change the rule/)).toBeInTheDocument();
  });
});

describe("RuleFormPage", () => {
  // 001-AC13: the live compile shows the error and underlines its position.
  it("marks the filter error at its line and column", async () => {
    mockApi({
      "/api/v1/rules/preview": (_: URL, init?: RequestInit) => {
        const { filter } = JSON.parse(String(init?.body)) as { filter: string };
        return filter.includes("nme")
          ? filterProblem(2, 9, "watch has no field nme")
          : Response.json({ items: [] });
      },
    });
    renderApp("/rules/new");
    const area = await screen.findByLabelText("Expression");
    expect(await screen.findByText("Empty: every event matches")).toBeInTheDocument();
    await userEvent.type(area, 'event.type == "x" &&{enter}  watch.nme');
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("watch has no field nme (line 2, column 9)");
    const mark = screen.getByTestId("filter-marker");
    expect(mark).toHaveAccessibleName("Line 2, column 9");
    expect(within(mark).getByText("n").tagName).toBe("MARK");
    expect(mark.textContent).toBe("  watch.nme\n        ^");
    expect(area).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByRole("button", { name: "Test against last 20 events" })).toBeDisabled();

    await userEvent.clear(area);
    await userEvent.type(area, "watch.name");
    expect(await screen.findByText("Valid filter")).toBeInTheDocument();
    expect(screen.queryByTestId("filter-marker")).not.toBeInTheDocument();
  });

  // 001-AC13: the preview table shows each recent event's outcome.
  it("tests the filter against recent events", async () => {
    const fn = mockApi({
      "/api/v1/watches": {
        // 009-AC10: every watch of the caller can be picked, a paused one too.
        items: [watch(), watch({ id: "w2", name: "Postgres", paused: true })],
      },
      "/api/v1/rules/preview": {
        items: [
          preview(3, true),
          preview(2, false),
          preview(1, false, "cannot compare string and int"),
        ],
      },
    });
    renderApp("/rules/new");
    await userEvent.click(await screen.findByRole("radio", { name: /^Selected watches/ }));
    const pick = await screen.findByRole("list", { name: "Watches to pick" });
    expect(within(pick).getByRole("checkbox", { name: /^Postgres/ })).toHaveAccessibleName(
      /Paused$/,
    );
    // 009-AC10: the rule form names no share.
    expect(document.body).not.toHaveTextContent(/shar/i);
    await userEvent.click(within(pick).getByRole("checkbox", { name: /^Go releases/ }));
    await userEvent.click(screen.getByRole("button", { name: "Test against last 20 events" }));
    const table = await screen.findByRole("table", { name: "Preview against recent events" });
    const rows = within(table).getAllByRole("row").slice(1);
    expect(rows.map((r) => r.textContent)).toEqual([
      expect.stringMatching(/v3 published.*✓ Match$/),
      expect.stringMatching(/v2 published.*✗ No match$/),
      expect.stringMatching(/v1 published.*Error$/),
    ]);
    expect(
      within(rows[2] as HTMLElement).getByTitle("cannot compare string and int"),
    ).toBeInTheDocument();
    expect(screen.getByText(/of 3 recent events match/)).toHaveTextContent(
      "1 of 3 recent events match.",
    );
    await waitFor(() =>
      expect(lastCall(fn, "/api/v1/rules/preview", "POST")?.body).toEqual({
        filter: "",
        watch_ids: ["w1"],
      }),
    );
  });

  it("shows an empty or failed preview", async () => {
    mockApi({ "/api/v1/rules/preview": { items: [] } });
    const first = renderApp("/rules/new");
    await userEvent.click(
      await screen.findByRole("button", { name: "Test against last 20 events" }),
    );
    expect(await screen.findByText(/No events yet in this scope/)).toBeInTheDocument();
    first.unmount();
    // No filter problem in the answer (5xx, 429): the filter could not be checked, so no test either.
    let answer = () => problem(500, "database down");
    mockApi({ "/api/v1/rules/preview": () => answer() });
    renderApp("/rules/new");
    expect(await screen.findByText(/Could not check the filter/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Test against last 20 events" })).toBeDisabled();
    answer = () => Response.json({ items: [] });
    await userEvent.type(screen.getByLabelText("Expression"), "watch.name");
    expect(await screen.findByText("Valid filter")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Test against last 20 events" })).toBeEnabled();
  });

  it("inserts a field at the cursor and creates the rule", async () => {
    const fn = mockApi({
      "/api/v1/channels": {
        items: [
          channel(),
          channel({ id: "c2", name: "Phone", type: "telegram", config: { chat_id: "-1" } }),
        ],
      },
      "/api/v1/rules/preview": { items: [] },
      "/api/v1/rules": (_: URL, init?: RequestInit) =>
        init?.method === "POST"
          ? Response.json(rule({ name: "New" }), { status: 201 })
          : Response.json({ items: [] }),
    });
    const { router } = renderApp("/rules/new");
    await userEvent.type(await screen.findByLabelText("Name"), "New");
    await userEvent.click(screen.getByRole("button", { name: "watch.kind" }));
    await waitFor(() => expect(screen.getByLabelText("Expression")).toHaveValue("watch.kind"));
    await userEvent.type(screen.getByLabelText("Expression"), ' == "oci_tags"');
    await userEvent.click(await screen.findByRole("checkbox", { name: /Phone/ }));
    expect(screen.getByText("Your inbox always gets matching events.")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Create rule" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/rules"));
    expect(lastCall(fn, "/api/v1/rules", "POST")?.body).toEqual({
      name: "New",
      filter: 'watch.kind == "oci_tags"',
      enabled: true,
      priority: "normal",
      channel_ids: ["c2"],
      watch_ids: [],
      delivery: "immediate",
    });
  });

  // 005-AC1: the form creates and edits a rule's priority, and shows the one stored.
  it("sets the priority", async () => {
    const fn = mockApi({
      "/api/v1/rules/preview": { items: [] },
      "/api/v1/rules": (_: URL, init?: RequestInit) =>
        init?.method === "POST"
          ? Response.json(rule({ id: "r9", priority: "high" }), { status: 201 })
          : Response.json({ items: [] }),
      "/api/v1/rules/r1": () => Response.json(rule({ priority: "high" })),
    });
    const { router } = renderApp("/rules/new");
    const select = await screen.findByRole("combobox", { name: "Priority" });
    expect(select).toHaveValue("normal");
    await userEvent.type(screen.getByLabelText("Name"), "New");
    await userEvent.selectOptions(select, "high");
    await userEvent.click(screen.getByRole("button", { name: "Create rule" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/rules"));
    expect(lastCall(fn, "/api/v1/rules", "POST")?.body).toMatchObject({ priority: "high" });
    expect(await screen.findByRole("heading", { level: 1, name: "Rules" })).toBeInTheDocument();

    await router.navigate("/rules/r1");
    expect(
      await screen.findByRole("heading", { level: 1, name: "Edit Releases" }),
    ).toBeInTheDocument();
    const stored = screen.getByRole("combobox", { name: "Priority" });
    expect(stored).toHaveValue("high");
    await userEvent.selectOptions(stored, "normal");
    await userEvent.click(screen.getByRole("button", { name: "Save rule" }));
    await waitFor(() =>
      expect(lastCall(fn, "/api/v1/rules/r1", "PATCH")?.body).toMatchObject({ priority: "normal" }),
    );
  });

  // 005-AC1: a refused priority shows on its field, not in the form's general errors.
  it("shows a priority error on its field", async () => {
    mockApi({
      "/api/v1/rules/preview": { items: [] },
      "/api/v1/rules": () =>
        problem(400, "invalid rule", [{ location: "body.priority", message: "unknown level" }]),
    });
    renderApp("/rules/new");
    await userEvent.click(await screen.findByRole("button", { name: "Create rule" }));
    expect(await screen.findByText("unknown level")).toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: "Priority" })).toHaveAttribute(
      "aria-invalid",
      "true",
    );
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("shows save errors on their fields", async () => {
    mockApi({
      "/api/v1/rules/preview": { items: [] },
      "/api/v1/rules": (_: URL, init?: RequestInit) =>
        init?.method === "POST"
          ? problem(400, "invalid rule", [
              { location: "body.name", message: "must not be blank" },
              { location: "body.channel_ids[0]", message: "no such channel among yours" },
            ])
          : Response.json({ items: [] }),
    });
    renderApp("/rules/new");
    await userEvent.click(await screen.findByRole("button", { name: "Create rule" }));
    expect(await screen.findByText("must not be blank")).toBeInTheDocument();
    expect(screen.getByRole("alert")).toHaveTextContent(
      "channel_ids[0]: no such channel among yours",
    );
    expect(screen.getByText(/No channels yet/)).toBeInTheDocument();
  });

  it("edits, then deletes, a rule", async () => {
    const fn = mockApi({
      "/api/v1/rules/r1": (_: URL, init?: RequestInit) =>
        init?.method === "DELETE"
          ? new Response(null, { status: 204 })
          : Response.json(
              rule({ watch_ids: ["w1"], watches: [{ id: "w1", name: "Go releases" }] }),
            ),
      "/api/v1/watches": { items: [watch()] },
      "/api/v1/channels": { items: [channel()] },
      "/api/v1/rules/preview": { items: [] },
    });
    const { router } = renderApp("/rules/r1");
    expect(await screen.findByRole("heading", { name: "Edit Releases" })).toBeInTheDocument();
    expect(await screen.findByRole("checkbox", { name: /Deploy bot/ })).toBeChecked();
    await userEvent.click(screen.getByRole("checkbox", { name: "Enabled" }));
    // 014-AC13: the rule opens on its scope, its watch picked.
    expect(screen.getByRole("radio", { name: /^Selected watches/ })).toBeChecked();
    expect(screen.getByRole("button", { name: "Remove Go releases" })).toBeInTheDocument();
    expect(
      within(await screen.findByRole("list", { name: "Watches to pick" })).getByRole("checkbox", {
        name: /^Go releases/,
      }),
    ).toBeChecked();
    await userEvent.click(screen.getByRole("radio", { name: /^All watches/ }));
    await userEvent.click(screen.getByRole("button", { name: "Save rule" }));
    await waitFor(() =>
      expect(lastCall(fn, "/api/v1/rules/r1", "PATCH")?.body).toEqual({
        name: "Releases",
        filter: 'event.type == "release.published"',
        enabled: false,
        priority: "normal",
        channel_ids: ["c1"],
        watch_ids: [],
        delivery: "immediate",
      }),
    );
    await waitFor(() => expect(router.state.location.pathname).toBe("/rules"));
    // Navigating back before the list rendered left the old form mounted: its Delete click opened nothing.
    expect(await screen.findByRole("heading", { level: 1, name: "Rules" })).toBeInTheDocument();

    await router.navigate("/rules/r1");
    await userEvent.click(await screen.findByRole("button", { name: "Delete rule" }));
    await userEvent.click(
      within(await screen.findByRole("dialog")).getByRole("button", { name: "Delete" }),
    );
    await waitFor(() => expect(router.state.location.pathname).toBe("/rules"));
    expect(lastCall(fn, "/api/v1/rules/r1", "DELETE")).toBeDefined();
  });

  it("shows not found, loading and load errors", async () => {
    mockApi({ "/api/v1/rules/r9": () => problem(404, "rule not found") });
    const first = renderApp("/rules/r9");
    expect(await screen.findByRole("heading", { name: "Page not found" })).toBeInTheDocument();
    first.unmount();
    mockApi({ "/api/v1/rules/r9": pending });
    const second = renderApp("/rules/r9");
    expect(screen.getByText("Loading…")).toBeInTheDocument();
    second.unmount();
    let calls = 0;
    mockApi({
      "/api/v1/rules/r9": () =>
        calls++ ? Response.json(rule({ id: "r9" })) : problem(500, "boom"),
      "/api/v1/rules/preview": { items: [] },
    });
    renderApp("/rules/r9");
    expect(await screen.findByRole("alert")).toHaveTextContent("The rule could not be loaded.");
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("heading", { name: "Edit Releases" })).toBeInTheDocument();
  });

  it("says when a delete fails", async () => {
    mockApi({
      "/api/v1/rules/r1": (_: URL, init?: RequestInit) =>
        init?.method === "DELETE" ? problem(500, "boom") : Response.json(rule()),
    });
    renderApp("/rules/r1");
    await userEvent.click(await screen.findByRole("button", { name: "Delete rule" }));
    await userEvent.click(
      within(await screen.findByRole("dialog")).getByRole("button", { name: "Delete" }),
    );
    expect(await screen.findByText(/Could not delete the rule/)).toBeInTheDocument();
    await userEvent.click(
      within(await screen.findByRole("dialog")).getByRole("button", { name: "Cancel" }),
    );
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  // A save error on the filter describes the old text: editing clears it.
  it("clears a save error once the filter changes", async () => {
    mockApi({
      "/api/v1/rules/preview": { items: [] },
      "/api/v1/rules": (_: URL, init?: RequestInit) =>
        init?.method === "POST"
          ? filterProblem(1, 1, "stale on save")
          : Response.json({ items: [] }),
    });
    renderApp("/rules/new");
    await userEvent.type(await screen.findByLabelText("Name"), "r");
    await userEvent.click(screen.getByRole("button", { name: "Create rule" }));
    expect(await screen.findByText("stale on save (line 1, column 1)")).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText("Expression"), "x");
    expect(screen.queryByText("stale on save (line 1, column 1)")).not.toBeInTheDocument();
  });

  // 012-AC13, 012-AC15: Digest shows its times and zone and says webhook and script channels stay immediate;
  // the schedule is checked before sending, as the server would.
  it("creates a digest rule with its times and time zone, checked before sending", async () => {
    const fn = mockApi({
      "/api/v1/rules/preview": { items: [] },
      "/api/v1/rules": (_: URL, init?: RequestInit) =>
        init?.method === "POST"
          ? Response.json(rule({ name: "New" }), { status: 201 })
          : Response.json({ items: [] }),
    });
    const { router } = renderApp("/rules/new");
    await userEvent.type(await screen.findByLabelText("Name"), "New");
    expect(screen.getByRole("radio", { name: /^Immediate/ })).toBeChecked();
    expect(screen.queryByRole("list", { name: "Digest times" })).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("radio", { name: /^Digest/ }));
    expect(
      screen.getByText(
        "Webhook and script channels still get each event at once: they have no digest format.",
      ),
    ).toBeInTheDocument();
    expect(screen.getByLabelText("Time 1")).toHaveValue("09:00");
    expect(screen.getByRole("button", { name: "Remove time 1" })).toBeDisabled();
    const zone = screen.getByLabelText("Time zone");
    expect(zone).toHaveValue(Intl.DateTimeFormat().resolvedOptions().timeZone);
    expect(zone).toHaveAccessibleDescription("The times are local to this zone.");

    // A duplicate, an empty row and an unknown zone are refused here, as the server would.
    await userEvent.click(screen.getByRole("button", { name: "Add a time" }));
    await userEvent.click(screen.getByRole("button", { name: "Add a time" }));
    await userEvent.type(screen.getByLabelText("Time 2"), "09:00");
    await userEvent.clear(zone);
    await userEvent.type(zone, "Mars/Olympus");
    await userEvent.click(screen.getByRole("button", { name: "Create rule" }));
    expect(screen.getByText("09:00 is already listed")).toBeInTheDocument();
    expect(screen.getByLabelText("Time 2")).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByLabelText("Time 3")).toHaveAttribute("aria-invalid", "true");
    // Each row is described by its own error only.
    expect(screen.getByLabelText("Time 1")).not.toHaveAttribute("aria-describedby");
    expect(screen.getByLabelText("Time 2")).toHaveAccessibleDescription("09:00 is already listed");
    expect(screen.getByLabelText("Time 3")).toHaveAccessibleDescription(
      "a local time as HH:MM, e.g. 09:00",
    );
    expect(screen.getByText("an IANA time zone, e.g. Europe/Paris")).toBeInTheDocument();
    expect(lastCall(fn, "/api/v1/rules", "POST")).toBeUndefined();

    await userEvent.click(screen.getByRole("button", { name: "Remove time 3" }));
    // The rows changed: their errors go with them.
    expect(screen.queryByText("09:00 is already listed")).not.toBeInTheDocument();
    expect(screen.getByLabelText("Time 2")).not.toHaveAttribute("aria-invalid");
    const second = screen.getByLabelText("Time 2");
    await userEvent.clear(second);
    await userEvent.type(second, "18:00");
    await userEvent.clear(zone);
    await userEvent.type(zone, "Europe/Paris");
    await userEvent.click(screen.getByRole("button", { name: "Create rule" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/rules"));
    expect(lastCall(fn, "/api/v1/rules", "POST")?.body).toMatchObject({
      name: "New",
      delivery: "digest",
      at: ["09:00", "18:00"],
      tz: "Europe/Paris",
    });
  });

  // 012-AC13: an empty zone is sent as UTC, 24 rows is the cap, and a server error lands on its row.
  it("caps the digest times and places the server's schedule errors", async () => {
    const fn = mockApi({
      "/api/v1/rules/preview": { items: [] },
      "/api/v1/rules": (_: URL, init?: RequestInit) =>
        init?.method === "POST"
          ? problem(400, "invalid rule", [
              { location: "body.at[0]", message: "server says no" },
              { location: "body.delivery", message: "digest refused" },
            ])
          : Response.json({ items: [] }),
    });
    renderApp("/rules/new");
    await userEvent.click(await screen.findByRole("radio", { name: /^Digest/ }));
    await userEvent.clear(screen.getByLabelText("Time zone"));
    const add = screen.getByRole("button", { name: "Add a time" });
    for (let i = 1; i < 24; i++) await userEvent.click(add);
    expect(add).toBeDisabled();
    for (let i = 24; i > 1; i--)
      await userEvent.click(screen.getByRole("button", { name: `Remove time ${i}` }));
    await userEvent.click(screen.getByRole("button", { name: "Create rule" }));
    expect(await screen.findByText("server says no")).toBeInTheDocument();
    expect(screen.getByText("digest refused")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    for (const radio of ["Immediate", "Digest"])
      expect(
        screen.getByRole("radio", { name: new RegExp(`^${radio}`) }),
      ).toHaveAccessibleDescription("digest refused");
    expect(screen.getByLabelText("Time 1")).toHaveAccessibleDescription("server says no");
    await userEvent.click(screen.getByRole("button", { name: "Add a time" }));
    expect(screen.queryByText("server says no")).not.toBeInTheDocument();
    expect(lastCall(fn, "/api/v1/rules", "POST")?.body).toMatchObject({
      delivery: "digest",
      at: ["09:00"],
      tz: "UTC",
    });
  });

  // 012-AC13, 012-AC18: the list names the schedule; editing shows it back, and Immediate drops at and tz.
  it("shows and edits a digest rule's schedule", async () => {
    const digest = rule({ delivery: "digest", at: ["09:00", "18:00"], tz: "Europe/Paris" });
    const fn = mockApi({
      "/api/v1/rules": { items: [digest] },
      "/api/v1/rules/r1": () => Response.json(digest),
      "/api/v1/rules/preview": { items: [] },
    });
    const { router } = renderApp("/rules");
    const list = await screen.findByRole("list", { name: "Rules" });
    expect(within(list).getByText("Digest at 09:00, 18:00 (Europe/Paris)")).toBeInTheDocument();
    await router.navigate("/rules/r1");
    expect(await screen.findByRole("radio", { name: /^Digest/ })).toBeChecked();
    expect(screen.getByLabelText("Time 1")).toHaveValue("09:00");
    expect(screen.getByLabelText("Time 2")).toHaveValue("18:00");
    expect(screen.getByLabelText("Time zone")).toHaveValue("Europe/Paris");
    await userEvent.click(screen.getByRole("radio", { name: /^Immediate/ }));
    expect(screen.queryByLabelText("Time zone")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save rule" }));
    await waitFor(() => expect(lastCall(fn, "/api/v1/rules/r1", "PATCH")).toBeDefined());
    const body = lastCall(fn, "/api/v1/rules/r1", "PATCH")?.body as Record<string, unknown>;
    expect(body.delivery).toBe("immediate");
    expect(body).not.toHaveProperty("at");
    expect(body).not.toHaveProperty("tz");
  });

  // 012-AC14: high priority locks the delivery to immediate, says why, and sends immediate.
  it("locks the delivery while the priority is high", async () => {
    const fn = mockApi({
      "/api/v1/rules/preview": { items: [] },
      "/api/v1/rules": (_: URL, init?: RequestInit) =>
        init?.method === "POST"
          ? Response.json(rule({ priority: "high" }), { status: 201 })
          : Response.json({ items: [] }),
    });
    const { router } = renderApp("/rules/new");
    await userEvent.type(await screen.findByLabelText("Name"), "Urgent");
    await userEvent.click(screen.getByRole("radio", { name: /^Digest/ }));
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Priority" }), "high");
    const why = "High priority sends each event at once: set priority to normal for a digest.";
    for (const radio of ["Immediate", "Digest"]) {
      const r = screen.getByRole("radio", { name: new RegExp(`^${radio}`) });
      expect(r).toBeDisabled();
      expect(r).toHaveAccessibleDescription(why);
    }
    expect(screen.getByRole("radio", { name: /^Immediate/ })).toBeChecked();
    expect(screen.queryByLabelText("Time zone")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Create rule" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/rules"));
    const body = lastCall(fn, "/api/v1/rules", "POST")?.body as Record<string, unknown>;
    expect(body).toMatchObject({ priority: "high", delivery: "immediate" });
    expect(body).not.toHaveProperty("at");
  });

  // 012-AC13: focus follows the rows: a new row takes it, a removed one hands it back.
  it("moves focus as digest times are added and removed", async () => {
    mockApi({ "/api/v1/rules/preview": { items: [] } });
    renderApp("/rules/new");
    await userEvent.click(await screen.findByRole("radio", { name: /^Digest/ }));
    const add = screen.getByRole("button", { name: "Add a time" });
    await userEvent.click(add);
    await userEvent.click(add);
    expect(document.activeElement).toBe(screen.getByLabelText("Time 3"));
    await userEvent.click(screen.getByRole("button", { name: "Remove time 3" }));
    expect(document.activeElement).toBe(screen.getByLabelText("Time 2"));
    await userEvent.click(screen.getByRole("button", { name: "Remove time 1" }));
    expect(document.activeElement).toBe(add);
    expect(
      within(screen.getByRole("list", { name: "Digest times" })).getAllByRole("listitem"),
    ).toHaveLength(1);
  });
});

describe("RuleFormPage live test", () => {
  const items = [preview(3, true), preview(2, false)];
  function editPage(
    test: (url: URL, init?: RequestInit) => Response | Promise<Response>,
    over: Partial<Rule> = {},
  ) {
    const fn = mockApi({
      "/api/v1/rules/r1": rule({
        watch_ids: ["w1"],
        watches: [{ id: "w1", name: "Go releases" }],
        ...over,
      }),
      "/api/v1/watches": { items: [watch()] },
      "/api/v1/channels": { items: [channel(), channel({ id: "c2", name: "Ops room" })] },
      "/api/v1/rules/preview": { items },
      "/api/v1/rules/r1/test": test,
    });
    renderApp("/rules/r1");
    return fn;
  }
  const openPreview = async () =>
    userEvent.click(await screen.findByRole("button", { name: "Test against last 20 events" }));
  const results = () => screen.getByRole("status", { name: "Live test results" });

  // 013-AC10: only a saved rule can be live tested.
  it("offers a live test per preview row on the edit page only", async () => {
    mockApi({ "/api/v1/rules/preview": { items } });
    const first = renderApp("/rules/new");
    await openPreview();
    await screen.findByRole("table", { name: "Preview against recent events" });
    expect(screen.queryByRole("button", { name: /^Live test/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("status", { name: "Live test results" })).not.toBeInTheDocument();
    first.unmount();
    editPage(() => Response.json({ items: [] }));
    await openPreview();
    const buttons = await screen.findAllByRole("button", { name: /^Live test: / });
    expect(buttons.map((b) => b.getAttribute("aria-label"))).toEqual([
      "Live test: v3 published",
      "Live test: v2 published",
    ]);
    expect(buttons[0]).toHaveAttribute("title", "Sends this event to the rule's channels now");
    expect(screen.queryByRole("status", { name: "Live test results" })).not.toBeInTheDocument();
  });

  // 013-AC10: each channel's outcome, a script's log lines, a Web Push's accepted count.
  it("shows per-channel results", async () => {
    const fn = editPage(() =>
      Response.json({
        items: [
          {
            channel_id: "c1",
            channel_name: "Teams card",
            ok: true,
            logs: ["card built", "posted"],
          },
          { channel_id: "c2", channel_name: "Phone", ok: true, accepted: 2 },
        ],
      }),
    );
    await openPreview();
    await userEvent.click(await screen.findByRole("button", { name: "Live test: v2 published" }));
    expect(await within(results()).findByText("Live test of v2 published")).toBeInTheDocument();
    expect(lastCall(fn, "/api/v1/rules/r1/test", "POST")?.body).toEqual({ event_id: 2 });
    const [script, push] = within(results()).getAllByRole("listitem") as HTMLElement[];
    expect(script).toHaveTextContent(/^Teams card\s*SentLogs · 2 lines/);
    expect(within(script as HTMLElement).getByText(/card built/)).toHaveTextContent(
      "card built posted",
    );
    expect(push).toHaveTextContent("PhoneSentAccepted by 2 devices");
    // Right under the clicked row, wherever it sits in the preview.
    const rows = screen.getAllByRole("row");
    const clicked = rows.findIndex((r) => within(r).queryByText("v2 published"));
    expect(rows[clicked + 1]).toContainElement(results());
  });

  // 013-AC10: a failed channel, 013-AC9: a rule without channel, 013-AC10: a 429 and its delay.
  it("reports a failed channel, no channel and a rate limit", async () => {
    let answer: () => Response = () =>
      Response.json({
        items: [
          { channel_id: "c1", channel_name: "Deploy bot", ok: true },
          { channel_id: "c2", channel_name: "Script", ok: false, error: "sandbox busy", logs: [] },
        ],
      });
    editPage(() => answer());
    await openPreview();
    const button = await screen.findByRole("button", { name: "Live test: v3 published" });
    await userEvent.click(button);
    await within(results()).findByText("Live test of v3 published");
    const [ok, failed] = within(results()).getAllByRole("listitem");
    expect(ok).toHaveTextContent(/^Deploy bot\s*Sent$/);
    expect(failed).toHaveTextContent(/^Script\s*Failedsandbox busyLogs · 0 linesNo log line\.$/);

    answer = () => Response.json({ items: [] });
    await userEvent.click(button);
    expect(
      await within(results()).findByText(/this rule has no channel, only your inbox/),
    ).toBeInTheDocument();
    expect(within(results()).queryByRole("list")).not.toBeInTheDocument();

    answer = () => problem(429, "you sent a test less than 10s ago: retry in 7 s");
    await userEvent.click(button);
    expect(await within(results()).findByText(/retry in 7 s/)).toBeInTheDocument();
  });

  // 013: one test at a time, and the form says the test ignores its unsaved edits.
  it("runs one test at a time, the clicked row says so", async () => {
    editPage(pending);
    await openPreview();
    const buttons = await screen.findAllByRole("button", { name: /^Live test: / });
    await userEvent.click(buttons[1] as HTMLElement);
    for (const b of buttons) expect(b).toBeDisabled();
    expect(buttons.map((b) => b.textContent)).toEqual(["Live test", "Sending…"]);
    expect(within(results()).getByText("Sending…")).toBeInTheDocument();
  });

  // 013: the test sends the saved rule; the hint follows only what it reads (name, scope, priority, channels).
  it("hints at unsaved changes a live test would not use", async () => {
    editPage(pending);
    await openPreview();
    await screen.findAllByRole("button", { name: /^Live test: / });
    const hint = () => screen.queryByText("Unsaved changes: a live test uses the saved rule.");
    expect(hint()).not.toBeInTheDocument();
    // Outside the live region: typing must not make screen readers announce it.
    await userEvent.type(screen.getByLabelText("Name"), " v2");
    expect(hint()).toBeInTheDocument();
    expect(
      screen.getByRole("table", { name: "Preview against recent events" }),
    ).not.toContainElement(hint());
    await userEvent.clear(screen.getByLabelText("Name"));
    // The server trims the name: a trailing space saves the same rule.
    await userEvent.type(screen.getByLabelText("Name"), "Releases ");
    expect(hint()).not.toBeInTheDocument();

    await userEvent.click(await screen.findByRole("checkbox", { name: /Deploy bot/ }));
    expect(hint()).toBeInTheDocument();
    await userEvent.click(screen.getByRole("checkbox", { name: /Deploy bot/ }));
    expect(hint()).not.toBeInTheDocument();

    await userEvent.selectOptions(screen.getByLabelText("Priority"), "high");
    expect(hint()).toBeInTheDocument();
    await userEvent.selectOptions(screen.getByLabelText("Priority"), "normal");
    expect(hint()).not.toBeInTheDocument();

    // 014-AC20: the preview follows the picks, the test the saved scope.
    await userEvent.click(screen.getByRole("radio", { name: /^All watches/ }));
    expect(hint()).toBeInTheDocument();
    await userEvent.click(screen.getByRole("radio", { name: /^Selected watches/ }));
    expect(hint()).not.toBeInTheDocument();

    // The test ignores the filter.
    await userEvent.type(screen.getByLabelText("Expression"), " && true");
    expect(hint()).not.toBeInTheDocument();
  });

  // 013: the channel order is not a change: unticking then reticking one leaves the rule as saved.
  it("gives no hint when the same channels are picked again", async () => {
    editPage(pending, { channel_ids: ["c1", "c2"] });
    await openPreview();
    await screen.findAllByRole("button", { name: /^Live test: / });
    const first = await screen.findByRole("checkbox", { name: /Deploy bot/ });
    await userEvent.click(first);
    expect(screen.getByText(/uses the saved rule/)).toBeInTheDocument();
    await userEvent.click(first);
    expect(screen.queryByText(/uses the saved rule/)).not.toBeInTheDocument();
  });
});

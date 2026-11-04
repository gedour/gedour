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
        watch_id: "w1",
        watch_name: "Postgres tags",
        filter: "",
        channel_ids: [],
        enabled: false,
      }),
      rule({ id: "r3", name: "Lost", watch_id: "w9", watch_name: null }),
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
    expect(items[2]).toHaveTextContent("A watch you can no longer read");
    const sw = screen.getByRole("switch", { name: "Tags enabled" });
    expect(sw).toHaveAttribute("aria-checked", "false");
    await userEvent.click(sw);
    expect(lastCall(fn, "/api/v1/rules/r2", "PATCH")?.body).toEqual({ enabled: true });
    expect(await screen.findByText("Tags enabled")).toBeInTheDocument();
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
    mockApi({ "/api/v1/rules": () => new Response("", { status: 500 }) });
    renderApp("/rules");
    expect(await screen.findByRole("alert")).toHaveTextContent("Rules could not be loaded.");
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
        items: [watch(), watch({ id: "w2", name: "Shared", permission: "ro", owner_name: "Bob" })],
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
    await screen.findByRole("option", { name: "Shared (shared by Bob)" });
    await userEvent.selectOptions(screen.getByLabelText("Watches"), "w1");
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
        watch_id: "w1",
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
      channel_ids: ["c2"],
    });
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
          : Response.json(rule({ watch_id: "w1" })),
      "/api/v1/watches": { items: [watch()] },
      "/api/v1/channels": { items: [channel()] },
      "/api/v1/rules/preview": { items: [] },
    });
    const { router } = renderApp("/rules/r1");
    expect(await screen.findByRole("heading", { name: "Edit Releases" })).toBeInTheDocument();
    expect(await screen.findByRole("checkbox", { name: /Deploy bot/ })).toBeChecked();
    await userEvent.click(screen.getByRole("checkbox", { name: "Enabled" }));
    await userEvent.selectOptions(screen.getByLabelText("Watches"), "");
    await userEvent.click(screen.getByRole("button", { name: "Save rule" }));
    await waitFor(() =>
      expect(lastCall(fn, "/api/v1/rules/r1", "PATCH")?.body).toEqual({
        name: "Releases",
        filter: 'event.type == "release.published"',
        enabled: false,
        channel_ids: ["c1"],
        watch_id: "",
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
    mockApi({ "/api/v1/rules/r9": () => problem(500, "boom") });
    renderApp("/rules/r9");
    expect(await screen.findByRole("alert")).toHaveTextContent("The rule could not be loaded.");
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
});

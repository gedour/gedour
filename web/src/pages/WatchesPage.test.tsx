import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { mockApi } from "@/test/api";
import { renderApp } from "@/test/render";
import { lastCall, pending, problem, watch } from "@/test/watches";

describe("WatchesPage", () => {
  it("runs all watches and reports how many were scheduled", async () => {
    let scheduled = 2;
    const fn = mockApi({
      "/api/v1/watches": { items: [watch()] },
      "/api/v1/watches/run": () => Response.json({ scheduled }, { status: 202 }),
    });
    renderApp("/watches");
    await userEvent.click(await screen.findByRole("button", { name: "Run all" }));
    expect(await screen.findByText("2 watches scheduled to run")).toBeInTheDocument();
    expect(lastCall(fn, "/api/v1/watches/run", "POST")).toBeDefined();
    scheduled = 0;
    await userEvent.click(screen.getByRole("button", { name: "Run all" }));
    expect(await screen.findByText("No watch to run")).toBeInTheDocument();
  });

  it("reports a failed run all", async () => {
    mockApi({
      "/api/v1/watches": { items: [watch()] },
      "/api/v1/watches/run": () =>
        Response.json({ title: "Internal Server Error", status: 500 }, { status: 500 }),
    });
    renderApp("/watches");
    await userEvent.click(await screen.findByRole("button", { name: "Run all" }));
    expect(await screen.findByText(/^Could not run the watches/)).toBeInTheDocument();
  });

  it("shows each watch's run status and next run", async () => {
    const soon = new Date(Date.now() + 14.5 * 60_000).toISOString();
    mockApi({
      "/api/v1/watches": {
        items: [
          watch({ id: "a", name: "A", last_status: "ok", next_run_at: soon }),
          watch({ id: "b", name: "B", last_status: "error", next_run_at: "2020-01-01T00:00:00Z" }),
          watch({ id: "c", name: "C", last_status: "rate_limited", next_run_at: soon }),
          watch({ id: "d", name: "D", last_status: "ok", paused: true }),
          watch({ id: "e", name: "E" }),
        ],
      },
    });
    renderApp("/watches");
    const rows = within(await screen.findByRole("list", { name: "Watches" })).getAllByRole("link");
    expect(rows.map((r) => r.textContent)).toEqual([
      expect.stringMatching(/^OKA.*Next in 14 minutes$/),
      expect.stringMatching(/^ErrorB.*Due now$/),
      expect.stringMatching(/^Rate limitedC.*Next in 14 minutes$/),
      expect.stringMatching(/^PausedD.*15m$/),
      expect.stringMatching(/^Pending first runE/),
    ]);
  });

  it("lists watches with kind, target, schedule and badges", async () => {
    mockApi({
      "/api/v1/watches": {
        items: [
          watch(),
          watch({
            id: "w2",
            name: "Postgres",
            kind: "oci_tags",
            config: { registry: "docker.io", repository: "library/postgres" },
            paused: true,
          }),
        ],
      },
    });
    renderApp("/watches");
    const list = await screen.findByRole("list", { name: "Watches" });
    const rows = within(list).getAllByRole("link");
    expect(rows[0]).toHaveTextContent("Go releases");
    expect(rows[0]).toHaveTextContent("golang/go releases");
    expect(rows[0]).toHaveAttribute("href", "/watches/w1");
    expect(rows[1]).toHaveTextContent("postgres tags");
    expect(rows[1]).toHaveTextContent("Paused");
    // 009-AC10: no "Shared by" badge.
    expect(list).not.toHaveTextContent("Shared by");
  });

  // 001-AC24: loading, empty and error states are distinct.
  it("shows skeletons while loading", () => {
    mockApi({ "/api/v1/watches": pending });
    renderApp("/watches");
    expect(screen.getAllByTestId("watch-skeleton")).toHaveLength(4);
  });

  it("shows the empty state with a New watch call to action", async () => {
    mockApi({ "/api/v1/watches": { items: [] } });
    renderApp("/watches");
    const empty = await screen.findByRole("region", { name: "No watches yet" });
    expect(within(empty).getByRole("link", { name: "New watch" })).toHaveAttribute(
      "href",
      "/watches/new",
    );
  });

  it("shows an error with a working retry", async () => {
    let fail = true;
    mockApi({
      "/api/v1/watches": () =>
        fail ? new Response("", { status: 500 }) : Response.json({ items: [watch()] }),
    });
    renderApp("/watches");
    expect(await screen.findByRole("alert")).toHaveTextContent("Watches could not be loaded.");
    fail = false;
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("Go releases")).toBeInTheDocument();
  });

  it("sends search and kind filters, and clears them", async () => {
    const fn = mockApi({
      "/api/v1/watches": (url: URL) =>
        Response.json({ items: url.searchParams.size > 1 ? [] : [watch()] }),
    });
    renderApp("/watches");
    await screen.findByText("Go releases");
    await userEvent.type(screen.getByLabelText("Search"), "go");
    await userEvent.selectOptions(screen.getByLabelText("Kind"), "github_pr");
    // 009-AC10: no ownership filter.
    expect(screen.queryByRole("group", { name: "Ownership" })).not.toBeInTheDocument();
    await waitFor(() => {
      const q = lastCall(fn, "/api/v1/watches")?.url.searchParams;
      expect(Object.fromEntries(q ?? [])).toEqual({ q: "go", kind: "github_pr" });
    });
    expect(await screen.findByText("No watches match these filters")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Clear filters" }));
    expect(screen.getByLabelText("Search")).toHaveValue("");
    expect(await screen.findByText("Go releases")).toBeInTheDocument();
  });

  it("marks every case-insensitive match of the search in names and targets", async () => {
    mockApi({
      "/api/v1/watches": (url: URL) =>
        Response.json({
          items: url.searchParams.get("q")
            ? [watch({ name: "Go%go", config: { repo: "golang/go" } })]
            : [watch()],
        }),
    });
    renderApp("/watches");
    await screen.findByText("Go releases");
    expect(document.querySelector("mark")).toBeNull();
    await userEvent.type(screen.getByLabelText("Search"), "GO");
    await waitFor(() =>
      expect([...document.querySelectorAll("mark")].map((m) => m.textContent)).toEqual([
        "Go",
        "go",
        "go",
        "go",
      ]),
    );
    await userEvent.clear(screen.getByLabelText("Search"));
    await userEvent.type(screen.getByLabelText("Search"), "%");
    await waitFor(() =>
      expect([...document.querySelectorAll("mark")].map((m) => m.textContent)).toEqual(["%"]),
    );
  });

  it("leaves a name unmarked when lowercasing changes its length", async () => {
    mockApi({
      "/api/v1/watches": { items: [watch({ name: "İstanbul go", config: { repo: "x/y" } })] },
    });
    renderApp("/watches");
    await screen.findByText("İstanbul go");
    await userEvent.type(screen.getByLabelText("Search"), "go");
    expect(screen.getByText("İstanbul go")).toBeInTheDocument();
    expect(document.querySelector("mark")).toBeNull();
  });

  it("deletes the selected watches after confirmation", async () => {
    let items = [
      watch({ id: "a", name: "A" }),
      watch({ id: "b", name: "B" }),
      // 009-AC10: a paused watch is selectable too.
      watch({ id: "c", name: "C", paused: true }),
    ];
    const del = (id: string) => () => {
      items = items.filter((w) => w.id !== id);
      return new Response(null, { status: 204 });
    };
    const fn = mockApi({
      "/api/v1/watches": () => Response.json({ items }),
      "/api/v1/watches/a": del("a"),
      "/api/v1/watches/b": del("b"),
    });
    renderApp("/watches");
    // 009-AC10: every tile is selectable.
    expect(await screen.findByRole("checkbox", { name: "Select: C" })).toBeEnabled();
    expect(screen.queryByRole("toolbar")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("checkbox", { name: "Select: B" }));
    let bar = screen.getByRole("toolbar", { name: "Bulk actions" });
    expect(bar).toHaveTextContent("1 selected");
    await userEvent.click(within(bar).getByRole("button", { name: "Select all 3" }));
    expect(screen.getByRole("checkbox", { name: "Select: A" })).toHaveAttribute(
      "aria-checked",
      "true",
    );
    expect(screen.getByRole("checkbox", { name: "Select: C" })).toHaveAttribute(
      "aria-checked",
      "true",
    );
    expect(bar).toHaveTextContent("3 selected");
    expect(within(bar).getByRole("button", { name: "Select none" })).toBeInTheDocument();
    await userEvent.click(within(bar).getByRole("button", { name: "Clear" }));
    expect(screen.queryByRole("toolbar")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("checkbox", { name: "Select: A" }));
    await userEvent.click(screen.getByRole("checkbox", { name: "Select: B" }));
    bar = screen.getByRole("toolbar", { name: "Bulk actions" });

    await userEvent.click(within(bar).getByRole("button", { name: "Delete" }));
    const dialog = screen.getByRole("dialog", { name: "Delete 2 watches?" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(lastCall(fn, "/api/v1/watches/a", "DELETE")).toBeUndefined();

    await userEvent.click(within(bar).getByRole("button", { name: "Delete" }));
    await userEvent.click(
      within(await screen.findByRole("dialog")).getByRole("button", { name: "Delete" }),
    );
    expect(await screen.findByRole("status")).toHaveTextContent("Deleted 2 watches");
    expect(lastCall(fn, "/api/v1/watches/b", "DELETE")).toBeDefined();
    await waitFor(() => expect(screen.queryByText("A")).not.toBeInTheDocument());
    expect(screen.getByText("C")).toBeInTheDocument();
    expect(screen.queryByRole("toolbar")).not.toBeInTheDocument();
  });

  it("names everything the delete removes", async () => {
    mockApi({ "/api/v1/watches": { items: [watch()] } });
    renderApp("/watches");
    await userEvent.click(await screen.findByRole("checkbox", { name: "Select: Go releases" }));
    await userEvent.click(screen.getByRole("button", { name: "Delete" }));
    const dialog = screen.getByRole("dialog", { name: "Delete 1 watch?" });
    expect(dialog).toHaveTextContent(
      "Deleting a watch also removes its events and every rule on it. Every inbox item and delivery for those events goes too. This cannot be undone.",
    );
    // 009-AC10: the delete dialog names no share.
    expect(dialog).not.toHaveTextContent(/shar/i);
  });

  it("keeps watches hidden by a filter selected but out of the count and the delete", async () => {
    const fn = mockApi({
      "/api/v1/watches": (url: URL) =>
        Response.json({
          items: url.searchParams.get("q")
            ? [watch({ id: "b", name: "B" })]
            : [watch({ id: "a", name: "A" }), watch({ id: "b", name: "B" })],
        }),
      "/api/v1/watches/b": () => new Response(null, { status: 204 }),
    });
    renderApp("/watches");
    await userEvent.click(await screen.findByRole("checkbox", { name: "Select: A" }));
    const search = screen.getByRole("searchbox", { name: "Search" });
    const hideA = async () => {
      await userEvent.type(search, "b");
      await waitFor(() => expect(screen.queryByText("A")).not.toBeInTheDocument());
    };
    await hideA();
    await userEvent.clear(search);
    expect(await screen.findByRole("checkbox", { name: "Select: A" })).toHaveAttribute(
      "aria-checked",
      "true",
    );
    await hideA();
    await userEvent.click(screen.getByRole("checkbox", { name: "Select: B" }));
    const bar = screen.getByRole("toolbar", { name: "Bulk actions" });
    expect(bar).toHaveTextContent("1 selected");
    expect(within(bar).getByRole("button", { name: "Select none" })).toBeInTheDocument();
    await userEvent.click(within(bar).getByRole("button", { name: "Delete" }));
    await userEvent.click(
      within(screen.getByRole("dialog", { name: "Delete 1 watch?" })).getByRole("button", {
        name: "Delete",
      }),
    );
    expect(await screen.findByRole("status")).toHaveTextContent("Deleted 1 watch");
    expect(lastCall(fn, "/api/v1/watches/a", "DELETE")).toBeUndefined();
  });

  it("holds the dialog open while the delete runs", async () => {
    mockApi({
      "/api/v1/watches": { items: [watch()] },
      "/api/v1/watches/w1": pending,
    });
    renderApp("/watches");
    await userEvent.click(await screen.findByRole("checkbox", { name: "Select: Go releases" }));
    await userEvent.click(screen.getByRole("button", { name: "Delete" }));
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByRole("button", { name: "Cancel" })).toBeEnabled();
    expect(fireEvent(dialog, new Event("cancel", { cancelable: true }))).toBe(true);
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    expect(within(dialog).getByRole("button", { name: "Cancel" })).toBeDisabled();
    expect(fireEvent(dialog, new Event("cancel", { cancelable: true }))).toBe(false);
    // A browser may close it past the cancelled Escape.
    (dialog as HTMLDialogElement).close();
    expect(screen.getByRole("dialog")).toHaveAttribute("open");
  });

  describe("when the toolbar leaves", () => {
    const withSelection = async () => {
      mockApi({
        "/api/v1/watches": {
          items: [watch({ id: "a", name: "A" }), watch({ id: "b", name: "B" })],
        },
        "/api/v1/watches/a": () => new Response(null, { status: 204 }),
        "/api/v1/watches/b": () => new Response(null, { status: 204 }),
      });
      renderApp("/watches");
      await userEvent.click(await screen.findByRole("checkbox", { name: "Select: A" }));
      await userEvent.click(screen.getByRole("button", { name: "Select all 2" }));
      return screen.getByRole("toolbar", { name: "Bulk actions" });
    };
    const newWatch = () => screen.getByRole("link", { name: "New watch" });

    for (const name of ["Clear", "Select none"]) {
      it(`focus moves to New watch on ${name}`, async () => {
        const bar = await withSelection();
        within(bar).getByRole("button", { name }).focus();
        await userEvent.keyboard("{Enter}");
        expect(screen.queryByRole("toolbar")).not.toBeInTheDocument();
        expect(newWatch()).toHaveFocus();
      });
    }

    it("focus moves to New watch when the delete removes the whole selection", async () => {
      const bar = await withSelection();
      await userEvent.click(within(bar).getByRole("button", { name: "Delete" }));
      await userEvent.click(
        within(screen.getByRole("dialog")).getByRole("button", { name: "Delete" }),
      );
      expect(await screen.findByRole("status")).toHaveTextContent("Deleted 2 watches");
      expect(newWatch()).toHaveFocus();
    });

    it("focus stays in the search field when a filter empties the selection", async () => {
      mockApi({
        "/api/v1/watches": (url: URL) =>
          Response.json({
            items: url.searchParams.get("q") ? [] : [watch({ id: "a", name: "A" })],
          }),
      });
      renderApp("/watches");
      await userEvent.click(await screen.findByRole("checkbox", { name: "Select: A" }));
      const search = screen.getByRole("searchbox", { name: "Search" });
      await userEvent.type(search, "x");
      await waitFor(() => expect(screen.queryByRole("toolbar")).not.toBeInTheDocument());
      expect(search).toHaveFocus();
    });

    it("the page does not take the focus on load", async () => {
      mockApi({ "/api/v1/watches": { items: [watch()] } });
      renderApp("/watches");
      await screen.findByRole("checkbox", { name: "Select: Go releases" });
      expect(document.body).toHaveFocus();
    });
  });

  it("keeps the watches it could not delete selected", async () => {
    mockApi({
      "/api/v1/watches": { items: [watch({ id: "a", name: "A" }), watch({ id: "b", name: "B" })] },
      "/api/v1/watches/a": () => new Response(null, { status: 204 }),
      "/api/v1/watches/b": () => problem(403, "nope"),
    });
    renderApp("/watches");
    await userEvent.click(await screen.findByRole("checkbox", { name: "Select: A" }));
    const b = screen.getByRole("checkbox", { name: "Select: B" });
    await userEvent.click(b);
    await userEvent.click(b);
    expect(b).toHaveAttribute("aria-checked", "false");
    await userEvent.click(b);
    await userEvent.click(screen.getByRole("button", { name: "Delete" }));
    await userEvent.click(
      within(await screen.findByRole("dialog")).getByRole("button", { name: "Delete" }),
    );
    expect(await screen.findByText("Could not delete 1 of 2: nope")).toBeInTheDocument();
    expect(screen.getByRole("checkbox", { name: "Select: A" })).toHaveAttribute(
      "aria-checked",
      "false",
    );
    expect(screen.getByRole("checkbox", { name: "Select: B" })).toHaveAttribute(
      "aria-checked",
      "true",
    );
  });
});

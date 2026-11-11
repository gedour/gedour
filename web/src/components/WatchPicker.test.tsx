import { configure, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { mockApi } from "@/test/api";
import { renderApp } from "@/test/render";
import { preview, rule } from "@/test/routing";
import { lastCall, problem, watch } from "@/test/watches";
import type { Watch } from "@/types/api";

// Hundreds of rows render slowly under the coverage run on a loaded machine.
configure({ asyncUtilTimeout: 3000 });

const pad = (i: number) => String(i).padStart(4, "0");

/** n watches named "Repo 0000"…, half GitHub (target org/repo-i), half OCI (lib/img-i). */
function many(n: number): Watch[] {
  return Array.from({ length: n }, (_, i) =>
    i % 2
      ? watch({
          id: `w${pad(i)}`,
          name: `Repo ${pad(i)}`,
          kind: "oci_tags",
          config: { registry: "ghcr.io", repository: `lib/img-${pad(i)}` },
        })
      : watch({
          id: `w${pad(i)}`,
          name: `Repo ${pad(i)}`,
          config: {
            repo: `org/repo-${pad(i)}`,
            include_prereleases: false,
            api_url: "https://api.github.com",
          },
        }),
  );
}

/** GET /api/v1/watches as the server answers it: q on name or target, kind, limit (default 500). */
function watchesRoute(all: Watch[]) {
  return (url: URL) => {
    const q = (url.searchParams.get("q") ?? "").toLowerCase();
    const kind = url.searchParams.get("kind");
    const limit = Number(url.searchParams.get("limit") ?? 500);
    const items = all
      .filter((w) => !kind || w.kind === kind)
      .filter((w) => !q || `${w.name} ${JSON.stringify(w.config)}`.toLowerCase().includes(q))
      .slice(0, limit);
    return Response.json({ items });
  };
}

const watchCalls = (fn: { mock: { calls: unknown[][] } }) =>
  fn.mock.calls
    .map(([input]) => new URL(String(input), "http://app.test"))
    .filter((u) => u.pathname === "/api/v1/watches");

async function openPicker(path = "/rules/new") {
  const user = userEvent.setup();
  const r = renderApp(path);
  await user.click(await screen.findByRole("radio", { name: /^Selected watches/ }));
  return { user, ...r };
}

const rows = () => screen.findByRole("list", { name: "Watches to pick" });
const count = () => screen.getByText(/^\d+ selected$/);
const picker = () => screen.getByRole("group", { name: "Watches" });

describe("WatchPicker", () => {
  // 014-AC15: 2000 watches, the first 100 shown and a note; the search narrows on name and target with
  // the term marked; the kind narrows further; nothing matching says so.
  it("shows the first 100, searches server side and highlights the term", async () => {
    const fn = mockApi({
      "/api/v1/watches": watchesRoute(many(2000)),
      "/api/v1/rules/preview": { items: [] },
    });
    const { user } = await openPicker();
    expect(within(await rows()).getAllByRole("checkbox")).toHaveLength(100);
    expect(screen.getByText("Showing the first 100 matches: refine the search")).toBeVisible();
    expect(watchCalls(fn).at(-1)?.searchParams.get("limit")).toBe("101");

    await user.type(screen.getByRole("searchbox", { name: "Search" }), "repo-001");
    await waitFor(() =>
      expect(
        within(screen.getByRole("list", { name: "Watches to pick" })).getAllByRole("checkbox"),
      ).toHaveLength(5),
    );
    expect(screen.queryByText(/Showing the first 100/)).not.toBeInTheDocument();
    const call = watchCalls(fn).at(-1);
    expect(call?.searchParams.get("q")).toBe("repo-001");
    // Debounced: one request for the typed term, none for each keystroke in between.
    expect(watchCalls(fn).filter((u) => u.searchParams.get("q") === "repo-00")).toHaveLength(0);
    const marks = within(await rows()).getAllByText("repo-001");
    expect(marks[0]?.tagName).toBe("MARK");

    await user.clear(screen.getByRole("searchbox", { name: "Search" }));
    await user.type(screen.getByRole("searchbox", { name: "Search" }), "img-19");
    await user.selectOptions(screen.getByRole("combobox", { name: "Kind" }), "github_repo");
    expect(await screen.findByText("No watch matches the search.")).toBeInTheDocument();
    expect(watchCalls(fn).at(-1)?.searchParams.get("kind")).toBe("github_repo");
    await user.selectOptions(screen.getByRole("combobox", { name: "Kind" }), "oci_tags");
    await waitFor(async () =>
      expect(within(await rows()).getAllByRole("checkbox")).toHaveLength(50),
    );
    expect(within(await rows()).getAllByRole("checkbox")[0]).toHaveAccessibleName(
      /^Repo 1901 ghcr\.io\/lib\/img-1901 tags$/,
    );
  });

  // 014-AC16: select all matching takes every match past the 100 shown and keeps earlier picks; clear
  // matching unpicks only the matches; selected only lists the picks without a request; the count follows.
  it("selects and clears every match, and lists the picks alone", async () => {
    const fn = mockApi({
      "/api/v1/watches": watchesRoute(many(2000)),
      "/api/v1/rules/preview": { items: [] },
    });
    const { user } = await openPicker();
    await user.click(within(await rows()).getByRole("checkbox", { name: /^Repo 0000/ }));
    expect(count()).toHaveTextContent("1 selected");
    await user.type(screen.getByRole("searchbox", { name: "Search" }), "repo 1");
    await waitFor(() => expect(watchCalls(fn).at(-1)?.searchParams.get("q")).toBe("repo 1"));
    await user.click(screen.getByRole("button", { name: "Select all matching" }));
    // Repo 1000…1999: 1000 matches is past 500, so nothing is picked.
    expect(await within(picker()).findByRole("status")).toHaveTextContent(
      "500 or more watches match: refine the search. Nothing was picked.",
    );
    expect(count()).toHaveTextContent("1 selected");
    expect(watchCalls(fn).at(-1)?.searchParams.get("limit")).toBe("500");
    // Clearing 1000 matches clears the first 500 and says so; the pick is not among them.
    await user.click(screen.getByRole("button", { name: "Clear matching" }));
    expect(await within(picker()).findByRole("status")).toHaveTextContent(
      "500 or more watches match: only the first 500 were cleared. Refine the search.",
    );
    expect(count()).toHaveTextContent("1 selected");

    await user.type(screen.getByRole("searchbox", { name: "Search" }), "1");
    await waitFor(() => expect(watchCalls(fn).at(-1)?.searchParams.get("q")).toBe("repo 11"));
    // Typing again drops the old notice.
    expect(within(picker()).getByRole("status")).toBeEmptyDOMElement();
    await user.click(screen.getByRole("button", { name: "Select all matching" }));
    // Repo 1100…1199: 100 matches added to the earlier pick, all of them, not only the rows on screen.
    await waitFor(() => expect(count()).toHaveTextContent("101 selected"));
    expect(
      within(await rows())
        .getAllByRole("checkbox")
        .every((c) => (c as HTMLInputElement).checked),
    ).toBe(true);

    await user.click(screen.getByRole("checkbox", { name: "Selected only" }));
    const before = fn.mock.calls.length;
    const picked = within(await rows()).getAllByRole("checkbox");
    expect(picked).toHaveLength(101);
    expect(picked[0]).toHaveAccessibleName(/^Repo 0000/);
    expect(fn.mock.calls.length).toBe(before);
    expect(screen.getByRole("button", { name: "Select all matching" })).toBeDisabled();
    await user.click(screen.getByRole("checkbox", { name: "Selected only" }));

    await user.click(screen.getByRole("button", { name: "Clear matching" }));
    await waitFor(() => expect(count()).toHaveTextContent("1 selected"));
    expect(screen.getByRole("button", { name: "Remove Repo 0000" })).toBeInTheDocument();
  });

  // 014-AC16: picks plus the new matches past 500 is refused too, and picks nothing.
  it("refuses a select that would pass 500 picks", async () => {
    const rs = many(2000);
    mockApi({
      "/api/v1/rules/r1": () =>
        Response.json(
          rule({
            watch_ids: rs.slice(0, 450).map((w) => w.id),
            watches: rs.slice(0, 450).map((w) => ({ id: w.id, name: w.name })),
          }),
        ),
      "/api/v1/watches": watchesRoute(rs),
      "/api/v1/rules/preview": { items: [] },
    });
    const user = userEvent.setup();
    renderApp("/rules/r1");
    expect(await screen.findByRole("radio", { name: /^Selected watches/ })).toBeChecked();
    expect(count()).toHaveTextContent("450 selected");
    // Repo 1000…1099: 100 new, 550 in all.
    await user.type(screen.getByRole("searchbox", { name: "Search" }), "repo 10");
    await waitFor(async () =>
      expect(within(await rows()).getAllByRole("checkbox")[0]).toHaveAccessibleName(/^Repo 1000/),
    );
    await user.click(screen.getByRole("button", { name: "Select all matching" }));
    expect(await within(picker()).findByRole("status")).toHaveTextContent(
      "That would pick 550 watches, and a rule takes 500 at most. Nothing was picked.",
    );
    expect(count()).toHaveTextContent("450 selected");
    // Matches already picked count once: Repo 0000…0099 is all picked, so it adds nothing and passes.
    await user.clear(screen.getByRole("searchbox", { name: "Search" }));
    await user.type(screen.getByRole("searchbox", { name: "Search" }), "repo 00");
    await waitFor(async () =>
      expect(within(await rows()).getAllByRole("checkbox")[0]).toHaveAccessibleName(/^Repo 0000/),
    );
    await user.click(screen.getByRole("button", { name: "Select all matching" }));
    await waitFor(() => expect(within(picker()).getByRole("status")).toBeEmptyDOMElement());
    expect(count()).toHaveTextContent("450 selected");
  });

  // 014-AC17: chips past 10 fold into "+N more", which unfolds; each removes its watch; a watch the user
  // no longer owns shows as such and can be removed.
  it("shows the picks as chips, folded past 10, a lost watch included", async () => {
    const rs = many(12);
    const fn = mockApi({
      "/api/v1/rules/r1": (_: URL, init?: RequestInit) =>
        init?.method === "PATCH"
          ? Response.json(rule())
          : Response.json(
              rule({
                watch_ids: [...rs.map((w) => w.id), "gone"],
                watches: [
                  { id: "gone", name: null },
                  ...rs.map((w) => ({ id: w.id, name: w.name })),
                ],
              }),
            ),
      "/api/v1/watches": watchesRoute(rs),
      "/api/v1/rules/preview": { items: [] },
    });
    const user = userEvent.setup();
    renderApp("/rules/r1");
    const chips = await screen.findByRole("list", { name: "Picked watches" });
    expect(within(chips).getAllByRole("button", { name: /^Remove / })).toHaveLength(10);
    const more = within(chips).getByRole("button", { name: "+3 more" });
    expect(more).toHaveAttribute("aria-expanded", "false");
    await user.click(more);
    expect(within(chips).getAllByRole("button", { name: /^Remove / })).toHaveLength(13);
    await user.click(within(chips).getByRole("button", { name: "Show fewer" }));
    expect(within(chips).getAllByRole("button", { name: /^Remove / })).toHaveLength(10);

    await user.click(screen.getByRole("button", { name: "Remove A watch you no longer own" }));
    await user.click(screen.getByRole("button", { name: "Remove Repo 0003" }));
    expect(count()).toHaveTextContent("11 selected");
    expect(within(await rows()).getByRole("checkbox", { name: /^Repo 0003/ })).not.toBeChecked();
    // No fold left at 10 or fewer, after one more goes.
    await user.click(within(await rows()).getByRole("checkbox", { name: /^Repo 0004/ }));
    expect(screen.queryByRole("button", { name: /more$/ })).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Save rule" }));
    await waitFor(() => expect(lastCall(fn, "/api/v1/rules/r1", "PATCH")).toBeDefined());
    const body = lastCall(fn, "/api/v1/rules/r1", "PATCH")?.body as Record<string, unknown>;
    expect([...(body.watch_ids as string[])].sort()).toEqual(
      rs.map((w) => w.id).filter((id) => !["w0003", "w0004"].includes(id)),
    );
    expect(body).not.toHaveProperty("watch_id");
  });

  // 014-AC15: a user with no watch at all gets a link to create one.
  it("has empty, loading and error states", async () => {
    mockApi({ "/api/v1/watches": { items: [] }, "/api/v1/rules/preview": { items: [] } });
    const first = await openPicker();
    expect(await screen.findByText(/You have no watch yet/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Create one" })).toHaveAttribute(
      "href",
      "/watches/new",
    );
    await first.user.click(screen.getByRole("checkbox", { name: "Selected only" }));
    expect(screen.getByText("No watch selected yet.")).toBeInTheDocument();
    first.unmount();

    let calls = 0;
    mockApi({
      "/api/v1/watches": () =>
        calls++ ? Response.json({ items: [watch()] }) : problem(500, "boom"),
      "/api/v1/rules/preview": { items: [] },
    });
    const second = await openPicker();
    expect(await screen.findByText("Watches could not be loaded.")).toBeInTheDocument();
    await second.user.click(screen.getByRole("button", { name: "Retry" }));
    expect(
      within(await rows()).getByRole("checkbox", { name: /^Go releases/ }),
    ).toBeInTheDocument();
    // A failed select names why.
    calls = 0;
    mockApi({
      "/api/v1/watches": () => problem(500, "boom"),
      "/api/v1/rules/preview": { items: [] },
    });
    await second.user.click(screen.getByRole("button", { name: "Select all matching" }));
    expect(await within(picker()).findByRole("status")).toHaveTextContent(
      "Could not load the matching watches: boom",
    );
  });

  // 014-AC13: "Selected watches" with no pick is blocked on the field; a server error on a pick lands there too.
  it("blocks a save with no watch picked, and places the server's scope errors", async () => {
    const fn = mockApi({
      "/api/v1/watches": { items: [watch()] },
      "/api/v1/rules/preview": { items: [] },
      "/api/v1/rules": (_: URL, init?: RequestInit) =>
        init?.method === "POST"
          ? problem(400, "invalid rule", [
              { location: "body.watch_ids[0]", message: "no such watch among yours" },
            ])
          : Response.json({ items: [] }),
    });
    const { user } = await openPicker();
    await user.type(screen.getByLabelText("Name"), "Some");
    await user.click(screen.getByRole("button", { name: "Test against last 20 events" }));
    expect(screen.getByText("Pick a watch to test against its events.")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Create rule" }));
    const why = "Pick at least one watch, or choose All watches.";
    expect(screen.getByText(why)).toBeInTheDocument();
    for (const name of [/^All watches/, /^Selected watches/])
      expect(screen.getByRole("radio", { name })).toHaveAccessibleDescription(why);
    expect(lastCall(fn, "/api/v1/rules", "POST")).toBeUndefined();

    await user.click(within(await rows()).getByRole("checkbox", { name: /^Go releases/ }));
    expect(screen.queryByText(why)).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Create rule" }));
    expect(await screen.findByText("Go releases: no such watch among yours")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(lastCall(fn, "/api/v1/rules", "POST")?.body).toMatchObject({ watch_ids: ["w1"] });
    expect(lastCall(fn, "/api/v1/rules", "POST")?.body).not.toHaveProperty("watch_id");
  });

  // 014-AC20: the open preview runs on the picked watches and again on each change of the selection.
  it("reruns the preview on the picked watches", async () => {
    const fn = mockApi({
      "/api/v1/watches": watchesRoute(many(3)),
      "/api/v1/rules/preview": { items: [] },
    });
    const { user } = await openPicker();
    const previews = () =>
      fn.mock.calls
        .filter(([input]) => String(input).endsWith("/api/v1/rules/preview"))
        .map(([, init]) => JSON.parse(String((init as RequestInit).body)).watch_ids as string[]);
    await waitFor(() => expect(previews()).toEqual([[]]));
    // Closed, the preview only compiles the filter: picks send nothing (the per-user budget is 2/s).
    await user.click(within(await rows()).getByRole("checkbox", { name: /^Repo 0001/ }));
    await new Promise((r) => setTimeout(r, 500));
    expect(previews()).toEqual([[]]);
    await user.click(within(await rows()).getByRole("checkbox", { name: /^Repo 0001/ }));
    await user.click(screen.getByRole("button", { name: "Test against last 20 events" }));
    await user.click(within(await rows()).getByRole("checkbox", { name: /^Repo 0002/ }));
    await user.click(within(await rows()).getByRole("checkbox", { name: /^Repo 0000/ }));
    await waitFor(() => expect(previews().at(-1)).toEqual(["w0000", "w0002"]));
    await user.click(within(await rows()).getByRole("checkbox", { name: /^Repo 0002/ }));
    await waitFor(() => expect(previews().at(-1)).toEqual(["w0000"]));
    await user.click(screen.getByRole("radio", { name: /^All watches/ }));
    await waitFor(() => expect(previews().at(-1)).toEqual([]));
  });

  // 014-AC16: a click right after typing acts on the typed search, not on the one the list still shows.
  it("selects the matches of the search just typed", async () => {
    const fn = mockApi({
      "/api/v1/watches": watchesRoute(many(2000)),
      "/api/v1/rules/preview": { items: [] },
    });
    const { user } = await openPicker();
    await rows();
    await user.type(screen.getByRole("searchbox", { name: "Search" }), "repo 10");
    await user.click(screen.getByRole("button", { name: "Select all matching" }));
    await waitFor(() => expect(count()).toHaveTextContent("100 selected"));
    const bulk = watchCalls(fn).filter((u) => u.searchParams.get("limit") === "500");
    expect(bulk.map((u) => u.searchParams.get("q"))).toEqual(["repo 10"]);
  });

  // 014-AC8: a watch the user no longer owns stays in the rule through an edit and a save.
  it("keeps a lost watch on save", async () => {
    const fn = mockApi({
      "/api/v1/rules/r1": (_: URL, init?: RequestInit) =>
        Response.json(
          init?.method === "PATCH"
            ? rule()
            : rule({
                watch_ids: ["gone", "w1"],
                watches: [
                  { id: "gone", name: null },
                  { id: "w1", name: "Go releases" },
                ],
              }),
        ),
      "/api/v1/watches": { items: [watch()] },
      "/api/v1/rules/preview": { items: [] },
    });
    const user = userEvent.setup();
    renderApp("/rules/r1");
    await user.type(await screen.findByLabelText("Name"), " renamed");
    await user.click(screen.getByRole("button", { name: "Save rule" }));
    await waitFor(() =>
      expect(lastCall(fn, "/api/v1/rules/r1", "PATCH")?.body).toMatchObject({
        name: "Releases renamed",
        watch_ids: ["gone", "w1"],
      }),
    );
  });

  // 014-AC13: a refused pick is named on the field; two lost watches read the same, so one line says it.
  it("names the watch of each server error", async () => {
    mockApi({
      "/api/v1/rules/r1": (_: URL, init?: RequestInit) =>
        init?.method === "PATCH"
          ? problem(400, "invalid rule", [
              { location: "body.watch_ids[0]", message: "no such watch among your watches" },
              { location: "body.watch_ids[1]", message: "no such watch among your watches" },
              { location: "body.watch_ids[2]", message: "no such watch among your watches" },
            ])
          : Response.json(
              rule({
                watch_ids: ["gone", "w1", "lost"],
                watches: [
                  { id: "gone", name: null },
                  { id: "w1", name: "Go releases" },
                  { id: "lost", name: null },
                ],
              }),
            ),
      "/api/v1/watches": { items: [watch()] },
      "/api/v1/rules/preview": { items: [] },
    });
    const user = userEvent.setup();
    renderApp("/rules/r1");
    await user.click(await screen.findByRole("button", { name: "Save rule" }));
    const radio = screen.getByRole("radio", { name: /^Selected watches/ });
    await waitFor(() =>
      expect(radio).toHaveAccessibleDescription(
        "A watch you no longer own: no such watch among your watches Go releases: no such watch among your watches",
      ),
    );
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  // 014-AC18: removing a chip or a "Selected only" row hands focus to the next one, the previous, then the
  // search box or the toggle.
  it("keeps focus in the picker as picks are removed", async () => {
    const rs = many(3);
    mockApi({
      "/api/v1/rules/r1": () =>
        Response.json(
          rule({
            watch_ids: rs.map((w) => w.id),
            watches: rs.map((w) => ({ id: w.id, name: w.name })),
          }),
        ),
      "/api/v1/watches": watchesRoute(rs),
      "/api/v1/rules/preview": { items: [] },
    });
    const user = userEvent.setup();
    renderApp("/rules/r1");
    const remove = (n: string) => screen.getByRole("button", { name: `Remove Repo ${n}` });
    await user.click(await screen.findByRole("button", { name: "Remove Repo 0001" }));
    expect(document.activeElement).toBe(remove("0002"));
    await user.click(remove("0002"));
    expect(document.activeElement).toBe(remove("0000"));
    await user.click(remove("0000"));
    expect(document.activeElement).toBe(screen.getByRole("searchbox", { name: "Search" }));

    for (const w of rs)
      await user.click(
        within(await rows()).getByRole("checkbox", { name: new RegExp(`^${w.name}`) }),
      );
    const toggle = screen.getByRole("checkbox", { name: "Selected only" });
    await user.click(toggle);
    const row = (n: string) =>
      within(screen.getByRole("list", { name: "Watches to pick" })).getByRole("checkbox", {
        name: new RegExp(`^Repo ${n}`),
      });
    await user.click(row("0000"));
    expect(document.activeElement).toBe(row("0001"));
    await user.click(row("0002"));
    expect(document.activeElement).toBe(row("0001"));
    await user.click(row("0001"));
    expect(document.activeElement).toBe(toggle);
    expect(screen.getByText("No watch selected yet.")).toBeInTheDocument();
  });

  // 014-AC17: "Selected only" shows a rule's picks with their kind, target and paused badge once a list
  // showed their rows; a rule without watches details still opens on its scope.
  it("shows the details of the rule's picks once seen", async () => {
    let r = rule({
      watch_ids: ["w1"],
      watches: [{ id: "w1", name: "Go releases" }],
    });
    mockApi({
      "/api/v1/rules/r1": () => Response.json(r),
      "/api/v1/watches": { items: [watch({ paused: true })] },
      "/api/v1/rules/preview": { items: [] },
    });
    const user = userEvent.setup();
    const first = renderApp("/rules/r1");
    await rows();
    await user.click(screen.getByRole("checkbox", { name: "Selected only" }));
    expect(within(await rows()).getByRole("checkbox")).toHaveAccessibleName(
      "Go releases golang/go releases Paused",
    );
    first.unmount();

    r = rule({ watch_ids: ["w1"], watches: null });
    renderApp("/rules/r1");
    expect(await screen.findByRole("radio", { name: /^Selected watches/ })).toBeChecked();
    expect(count()).toHaveTextContent("1 selected");
  });

  // 014-AC20: the preview shows as stale while a new selection waits for its result.
  it("marks the preview stale while the selection settles", async () => {
    mockApi({
      "/api/v1/watches": watchesRoute(many(2)),
      "/api/v1/rules/preview": { items: [preview(1, true)] },
    });
    const { user } = await openPicker();
    await user.click(within(await rows()).getByRole("checkbox", { name: /^Repo 0000/ }));
    await user.click(screen.getByRole("button", { name: "Test against last 20 events" }));
    const table = await screen.findByRole("table", { name: "Preview against recent events" });
    const box = table.parentElement?.parentElement as HTMLElement;
    await waitFor(() => expect(box).not.toHaveAttribute("aria-busy"));
    await user.click(within(await rows()).getByRole("checkbox", { name: /^Repo 0001/ }));
    expect(box).toHaveAttribute("aria-busy", "true");
    expect(box).toHaveClass("opacity-60");
    await waitFor(() => expect(box).not.toHaveAttribute("aria-busy"));
  });

  // 014-AC16: picking row by row stops at 500: the other rows are disabled and say why; a rule over 500 is
  // refused on save with the count to unpick.
  it("caps the picks at 500 row by row and on save", async () => {
    const rs = many(600);
    let listed = 500;
    const fn = mockApi({
      "/api/v1/rules/r1": () =>
        Response.json(
          rule({
            watch_ids: rs.slice(0, listed).map((w) => w.id),
            watches: rs.slice(0, listed).map((w) => ({ id: w.id, name: w.name })),
          }),
        ),
      "/api/v1/watches": watchesRoute(rs),
      "/api/v1/rules/preview": { items: [] },
    });
    const user = userEvent.setup();
    const first = renderApp("/rules/r1");
    await user.type(await screen.findByRole("searchbox", { name: "Search" }), "repo 05");
    const free = await waitFor(() =>
      within(screen.getByRole("list", { name: "Watches to pick" })).getByRole("checkbox", {
        name: /^Repo 0500/,
      }),
    );
    const why = "500 watches picked, the most a rule takes: unpick one to pick another.";
    expect(free).toBeDisabled();
    expect(free).toHaveAccessibleDescription(why);
    const taken = within(await rows()).getByRole("checkbox", { name: /^Repo 0599/ });
    expect(taken).toBeDisabled();
    await user.clear(screen.getByRole("searchbox", { name: "Search" }));
    await user.type(screen.getByRole("searchbox", { name: "Search" }), "repo 04");
    const one = await waitFor(() =>
      within(screen.getByRole("list", { name: "Watches to pick" })).getByRole("checkbox", {
        name: /^Repo 0400/,
      }),
    );
    expect(one).toBeEnabled();
    await user.click(one);
    expect(count()).toHaveTextContent("499 selected");
    expect(screen.queryByText(why)).not.toBeInTheDocument();
    first.unmount();

    listed = 501;
    renderApp("/rules/r1");
    await user.click(await screen.findByRole("button", { name: "Save rule" }));
    expect(
      await screen.findByText("A rule takes 500 watches at most: unpick 1."),
    ).toBeInTheDocument();
    expect(lastCall(fn, "/api/v1/rules/r1", "PATCH")).toBeUndefined();
  });
});

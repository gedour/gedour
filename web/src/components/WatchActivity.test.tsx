import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { navigation } from "@/api/client";
import { mockApi } from "@/test/api";
import { renderApp } from "@/test/render";
import { lastCall, pending, problem, watch } from "@/test/watches";
import type { StateSnapshot, Watch, WatchEvent } from "@/types/api";

beforeEach(() => {
  vi.spyOn(navigation, "toLogin").mockImplementation(() => {});
});

const event = (id: number, type: string, payload: Record<string, unknown>): WatchEvent => ({
  id,
  type,
  occurred_at: id % 2 ? "2026-11-03T10:00:00Z" : null,
  observed_at: "2026-11-03T10:05:00Z",
  payload,
});

const snapshot = (id: number, state: unknown): StateSnapshot => ({
  id,
  observed_at: "2026-11-03T10:05:00Z",
  state,
});

function routes(w: Watch, extra: Record<string, unknown> = {}) {
  return {
    [`/api/v1/watches/${w.id}`]: w,
    [`/api/v1/watches/${w.id}/shares`]: { items: [] },
    [`/api/v1/watches/${w.id}/events`]: { items: [] },
    [`/api/v1/watches/${w.id}/states`]: { items: [] },
    ...extra,
  };
}

describe("StatusCard", () => {
  it("shows a pending first run", async () => {
    mockApi(routes(watch()));
    renderApp("/watches/w1");
    const card = await screen.findByRole("region", { name: "Status" });
    expect(card).toHaveTextContent("Pending first run");
    expect(card).toHaveTextContent("Last runNever");
    expect(await screen.findByText("Waiting for the first run")).toBeInTheDocument();
  });

  it("shows an error with its expandable message and the failure count", async () => {
    const long =
      "GET https://api.github.com/repos/a/b/releases: HTTP 502: Bad gateway\nretry later";
    mockApi(
      routes(
        watch({
          last_status: "error",
          last_error: long,
          consecutive_failures: 3,
          last_run_at: "2026-11-03T10:00:00Z",
        }),
      ),
    );
    renderApp("/watches/w1");
    const card = await screen.findByRole("region", { name: "Status" });
    expect(card).toHaveTextContent("Error");
    expect(card).toHaveTextContent("3 failures in a row");
    const summary = within(card).getByText(/HTTP 502: Bad gateway$/);
    await userEvent.click(summary);
    expect(within(card).getByText(/retry later/)).toBeVisible();
    expect(screen.getByText("No changes yet")).toBeInTheDocument();
  });

  it("labels a rate-limit message and a paused watch", async () => {
    mockApi(
      routes(
        watch({
          paused: true,
          last_status: "rate_limited",
          last_error: "rate limited by https://api.github.com until 2026-11-04T13:00:00Z",
        }),
      ),
    );
    renderApp("/watches/w1");
    const card = await screen.findByRole("region", { name: "Status" });
    expect(card).toHaveTextContent("Message");
    expect(card).toHaveTextContent("Next runPaused");
    expect(within(card).getByRole("button", { name: "Run now" })).toBeDisabled();
  });

  // Run now: scheduling, then running until the watch's last run moves, then refreshed events.
  it("runs now and waits for the run", async () => {
    let calls = 0;
    let release: () => void = () => {};
    const done = new Promise<void>((r) => (release = r));
    const before = watch({ last_status: "ok", last_run_at: "2026-11-03T10:00:00Z" });
    const after = { ...before, last_run_at: "2026-11-04T10:00:00Z" };
    let events: WatchEvent[] = [];
    const fn = mockApi(
      routes(before, {
        "/api/v1/watches/w1": () => {
          calls++;
          return Response.json(calls > 2 ? after : before);
        },
        "/api/v1/watches/w1/run": async () => {
          await done;
          return Response.json(before, { status: 202 });
        },
        "/api/v1/watches/w1/events": () => Response.json({ items: events }),
      }),
    );
    renderApp("/watches/w1");
    const button = await screen.findByRole("button", { name: "Run now" });
    await userEvent.click(button);
    expect(await screen.findByRole("button", { name: "Scheduling…" })).toBeDisabled();
    events = [event(1, "release.published", { tag: "v9" })];
    release();
    expect(await screen.findByText("Run of Go releases scheduled")).toBeInTheDocument();
    expect(await screen.findByText("v9 published", {}, { timeout: 4000 })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Run now" })).toBeEnabled();
    expect(lastCall(fn, "/api/v1/watches/w1/run", "POST")).toBeDefined();
  });

  // A leased watch reads as running and is polled until its run lands, then the timeline refreshes.
  it("shows a run in flight and refreshes when it lands", async () => {
    let calls = 0;
    const busy = watch({ running: true, last_status: "ok", last_run_at: "2026-11-03T10:00:00Z" });
    const done = { ...busy, running: false, last_run_at: "2026-11-04T10:00:00Z" };
    mockApi(
      routes(busy, {
        "/api/v1/watches/w1": () => Response.json(++calls > 1 ? done : busy),
        "/api/v1/watches/w1/events": () =>
          Response.json({ items: calls > 1 ? [event(1, "tag.created", { tag: "2.0" })] : [] }),
      }),
    );
    renderApp("/watches/w1");
    const card = await screen.findByRole("region", { name: "Status" });
    expect(card).toHaveTextContent("Next runrunning now");
    expect(within(card).getByRole("button", { name: "Running…" })).toBeDisabled();
    expect(await screen.findByText("tag 2.0 created", {}, { timeout: 4000 })).toBeInTheDocument();
    expect(within(card).getByRole("button", { name: "Run now" })).toBeEnabled();
  });

  // The "waiting for the run" timer is replaced by a new run-now and cleared on unmount.
  it("clears its run timer", async () => {
    const set = vi.spyOn(globalThis, "setTimeout");
    const clear = vi.spyOn(globalThis, "clearTimeout");
    let calls = 0;
    const base = watch({ last_status: "ok", last_run_at: "2026-11-03T10:00:00Z" });
    mockApi(
      routes(base, {
        // Each refetch reports a newer run, so the button comes back at once.
        "/api/v1/watches/w1": () =>
          Response.json({ ...base, last_run_at: `2026-11-04T10:00:0${calls++}Z` }),
        "/api/v1/watches/w1/run": () => Response.json(base, { status: 202 }),
      }),
    );
    const view = renderApp("/watches/w1");
    const timers = () =>
      set.mock.calls.flatMap((c, i) => (c[1] === 90_000 ? [set.mock.results[i]?.value] : []));
    await userEvent.click(await screen.findByRole("button", { name: "Run now" }));
    await waitFor(() => expect(timers()).toHaveLength(1));
    await userEvent.click(await screen.findByRole("button", { name: "Run now" }));
    await waitFor(() => expect(timers()).toHaveLength(2));
    expect(clear).toHaveBeenCalledWith(timers()[0]);
    view.unmount();
    expect(clear).toHaveBeenCalledWith(timers()[1]);
  });

  it("reports a refused run and hides the button from read-only sharees", async () => {
    mockApi(
      routes(watch(), {
        "/api/v1/watches/w1/run": () => problem(409, "the watch is paused: resume it to run it"),
      }),
    );
    const first = renderApp("/watches/w1");
    await userEvent.click(await screen.findByRole("button", { name: "Run now" }));
    expect(
      await screen.findByText(/Could not run the watch: the watch is paused/),
    ).toBeInTheDocument();
    first.unmount();

    mockApi(routes(watch({ permission: "ro", owner_name: "Alice" })));
    renderApp("/watches/w1");
    await screen.findByRole("region", { name: "Status" });
    expect(screen.queryByRole("button", { name: "Run now" })).not.toBeInTheDocument();
  });
});

describe("EventsTimeline", () => {
  it("lists events newest first with links and loads more", async () => {
    const fn = mockApi(
      routes(watch({ last_run_at: "2026-11-03T10:00:00Z" }), {
        "/api/v1/watches/w1/events": (url: URL) =>
          Response.json(
            url.searchParams.get("cursor")
              ? { items: [event(1, "tag.removed", { tag: "10" })] }
              : {
                  items: [
                    event(3, "release.published", {
                      tag: "v1.24.0",
                      url: "https://github.com/x/y/releases/tag/v1.24.0",
                    }),
                    event(2, "mr.state_changed", { iid: 12, from: "opened", to: "merged" }),
                  ],
                  next_cursor: "2",
                },
          ),
      }),
    );
    renderApp("/watches/w1");
    const list = await screen.findByRole("list", { name: "Events" });
    const items = within(list).getAllByRole("listitem");
    expect(items[0]).toHaveTextContent("Publishedv1.24.0 published");
    expect(items[1]).toHaveTextContent("MR !12 merged");
    expect(
      within(items[0] as HTMLElement).getByRole("link", {
        name: "Open v1.24.0 published upstream",
      }),
    ).toHaveAttribute("href", "https://github.com/x/y/releases/tag/v1.24.0");
    expect(within(items[0] as HTMLElement).getByRole("time")).toHaveAttribute(
      "dateTime",
      "2026-11-03T10:00:00Z",
    );
    expect(within(items[1] as HTMLElement).getByRole("time")).toHaveAttribute(
      "dateTime",
      "2026-11-03T10:05:00Z",
    );
    await userEvent.click(screen.getByRole("button", { name: "Load more" }));
    expect(await within(list).findByText("tag 10 removed")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Load more" })).not.toBeInTheDocument();
    expect(lastCall(fn, "/api/v1/watches/w1/events")?.url.searchParams.get("cursor")).toBe("2");
  });

  it("shows loading and error states", async () => {
    mockApi(routes(watch(), { "/api/v1/watches/w1/events": pending }));
    const first = renderApp("/watches/w1");
    expect(await screen.findByLabelText("Loading events")).toBeInTheDocument();
    first.unmount();

    mockApi(
      routes(watch(), { "/api/v1/watches/w1/events": () => new Response("", { status: 500 }) }),
    );
    renderApp("/watches/w1");
    expect(await screen.findByText("Events could not be loaded.")).toBeInTheDocument();
  });
});

async function openState() {
  await userEvent.click(await screen.findByRole("tab", { name: "State" }));
}

describe("StatePanel", () => {
  it("renders releases and the history", async () => {
    mockApi(
      routes(watch(), {
        "/api/v1/watches/w1/states": {
          items: [
            snapshot(2, {
              releases: [
                {
                  tag: "v2.0.0",
                  name: "Two",
                  prerelease: false,
                  draft: true,
                  published_at: null,
                  url: "",
                },
                {
                  tag: "v1.1.0-rc.1",
                  name: "RC",
                  prerelease: true,
                  draft: false,
                  published_at: "2026-11-01T00:00:00Z",
                  url: "https://github.com/x/y/releases/tag/v1.1.0-rc.1",
                },
              ],
            }),
            snapshot(1, { releases: null }),
          ],
        },
      }),
    );
    renderApp("/watches/w1");
    await openState();
    const table = await screen.findByRole("table");
    expect(table).toHaveTextContent("v2.0.0draft");
    expect(table).toHaveTextContent("v1.1.0-rc.1pre-release");
    expect(within(table).getByRole("link", { name: "v1.1.0-rc.1" })).toHaveAttribute(
      "target",
      "_blank",
    );
    const history = screen.getByRole("list", { name: "State history" });
    expect(within(history).getAllByRole("listitem")[0]).toHaveTextContent(
      "2 releases, latest v1.1.0-rc.1current",
    );
    expect(within(history).getAllByRole("listitem")[1]).toHaveTextContent("0 releases");
  });

  it("renders a PR and an MR summary", async () => {
    mockApi(
      routes(watch({ kind: "github_pr" }), {
        "/api/v1/watches/w1/states": {
          items: [
            snapshot(1, {
              number: 12,
              state: "open",
              draft: true,
              title: "Fix it",
              review_decision: "changes_requested",
              ci: "failure",
              head_sha: "0123456789abcdef",
              url: "https://github.com/a/b/pull/12",
            }),
          ],
        },
      }),
    );
    const first = renderApp("/watches/w1");
    await openState();
    expect(await screen.findByRole("link", { name: "Fix it" })).toBeInTheDocument();
    const panel = screen.getByRole("tabpanel");
    expect(panel).toHaveTextContent("open (draft)");
    expect(panel).toHaveTextContent("changes requested");
    expect(panel).toHaveTextContent("0123456789ab");
    first.unmount();

    mockApi(
      routes(watch({ kind: "gitlab_mr" }), {
        "/api/v1/watches/w1/states": {
          items: [
            snapshot(1, {
              iid: 7,
              state: "merged",
              draft: false,
              title: "Ship",
              approvals: { approved: true, count: 2 },
              pipeline: "success",
              sha: "fedcba9876543210",
              url: "",
            }),
          ],
        },
      }),
    );
    renderApp("/watches/w1");
    await openState();
    const mr = await screen.findByRole("tabpanel");
    await waitFor(() => expect(mr).toHaveTextContent("approved · 2"));
    expect(mr).toHaveTextContent("!7Ship");
    expect(within(mr).queryByRole("link", { name: "Ship" })).not.toBeInTheDocument();
  });

  // 001-AC12: the tag list is searchable and marks the highest semver.
  it("renders tags with search", async () => {
    const tags = Array.from({ length: 320 }, (_, i) => `1.${i}`);
    mockApi(
      routes(watch({ kind: "oci_tags" }), {
        "/api/v1/watches/w1/states": {
          items: [snapshot(1, { tags, count: 320, highest: "1.319" })],
        },
      }),
    );
    renderApp("/watches/w1");
    await openState();
    const list = await screen.findByRole("list", { name: "Tags" });
    expect(within(list).getAllByRole("listitem")).toHaveLength(300);
    expect(within(list).getAllByRole("listitem")[0]).toHaveTextContent("1.319");
    expect(screen.getByText("20 more; narrow the search to see them.")).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText("Search tags"), "1.31");
    expect(
      within(list)
        .getAllByRole("listitem")
        .map((li) => li.textContent),
    ).toEqual([
      "1.319",
      "1.318",
      "1.317",
      "1.316",
      "1.315",
      "1.314",
      "1.313",
      "1.312",
      "1.311",
      "1.310",
      "1.31",
    ]);
    await userEvent.type(screen.getByLabelText("Search tags"), "zzz");
    expect(screen.getByText("No tag matches “1.31zzz”.")).toBeInTheDocument();
  });

  it("shows empty, loading and error states", async () => {
    mockApi(routes(watch()));
    const a = renderApp("/watches/w1");
    await openState();
    expect(await screen.findByText("No state recorded yet")).toBeInTheDocument();
    a.unmount();

    mockApi(routes(watch(), { "/api/v1/watches/w1/states": pending }));
    const b = renderApp("/watches/w1");
    await openState();
    expect(await screen.findByLabelText("Loading state")).toBeInTheDocument();
    b.unmount();

    mockApi(
      routes(watch(), { "/api/v1/watches/w1/states": () => new Response("", { status: 500 }) }),
    );
    renderApp("/watches/w1");
    await openState();
    expect(await screen.findByText("The state could not be loaded.")).toBeInTheDocument();
  });
});

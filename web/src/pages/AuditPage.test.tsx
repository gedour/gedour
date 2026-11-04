import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { navigation } from "@/api/client";
import { actionTone } from "@/lib/audit";
import { mockApi } from "@/test/api";
import { renderApp } from "@/test/render";
import type { AuditItem } from "@/types/api";

const entry = (id: number, over: Partial<AuditItem> = {}): AuditItem => ({
  id,
  at: new Date(Date.now() - id * 60_000).toISOString(),
  actor_user_id: "u1",
  actor_name: "Alice",
  action: "user.login",
  target_type: "user",
  target_id: "11111111-2222-3333-4444-555555555555",
  before: null,
  after: { session: "abcd" },
  request_id: "req-1",
  ...over,
});

const pending = () => new Promise<Response>(() => {});

beforeEach(() => {
  vi.spyOn(navigation, "toLogin").mockImplementation(() => {});
});

describe("AuditPage", () => {
  // 001-AC22: entries render as a table, newest first, with action badge and target.
  it("lists entries in a table", async () => {
    const first = entry(1);
    mockApi({
      "/api/v1/audit": {
        items: [first, entry(2, { action: "user.created", target_id: "short", actor_name: "" })],
      },
    });
    renderApp("/audit");
    await screen.findByText("Alice");
    const table = screen.getByRole("table", { name: "Audit entries, newest first" });
    const rows = within(table).getAllByRole("row");
    expect(rows).toHaveLength(3);
    expect(rows[1]).toHaveTextContent("1 minute ago");
    expect(rows[1]).toHaveTextContent("Alice");
    expect(rows[1]).toHaveTextContent("11111111…");
    expect(rows[2]).toHaveTextContent("System");
    expect(rows[2]).toHaveTextContent("short");
    expect(within(table).getAllByRole("time")[0]).toHaveAttribute("datetime", first.at);
  });

  it("shows skeleton rows while loading", () => {
    mockApi({ "/api/v1/audit": pending });
    renderApp("/audit");
    expect(screen.getAllByTestId("audit-skeleton")).toHaveLength(5);
    expect(screen.getByRole("table")).toHaveAttribute("aria-busy", "true");
  });

  it("expands a row into a before/after diff", async () => {
    mockApi({
      "/api/v1/audit": {
        items: [
          entry(1, {
            action: "watch.updated",
            before: { name: "a", gone: 1 },
            after: { name: "b" },
          }),
        ],
      },
    });
    renderApp("/audit");
    const toggle = await screen.findByRole("button", { name: "Show details of watch.updated" });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    await userEvent.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    const diff = screen.getByRole("table", { name: "Changes recorded by watch.updated" });
    expect(within(diff).getByRole("rowheader", { name: "name" }).closest("tr")).toHaveTextContent(
      "nameab",
    );
    expect(within(diff).getByRole("rowheader", { name: "gone" }).closest("tr")).toHaveAttribute(
      "data-kind",
      "removed",
    );
    expect(screen.getByText("req-1")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Hide details of watch.updated" }));
    expect(screen.queryByRole("table", { name: /Changes recorded/ })).not.toBeInTheDocument();
  });

  it("says when an entry has no recorded state", async () => {
    mockApi({
      "/api/v1/audit": { items: [entry(1, { after: null, target_id: "", request_id: "" })] },
    });
    renderApp("/audit");
    await userEvent.click(await screen.findByRole("button", { name: /Show details/ }));
    expect(screen.getByText("No state recorded for this entry.")).toBeInTheDocument();
  });

  // 001-AC22: filters by target type and date go to the API.
  it("filters by target type and date range", async () => {
    const fn = mockApi({ "/api/v1/audit": { items: [entry(1)] } });
    renderApp("/audit");
    await screen.findByText("Alice");
    await userEvent.selectOptions(screen.getByLabelText("Target type"), "session");
    await userEvent.type(screen.getByLabelText("From"), "2026-01-01");
    await userEvent.type(screen.getByLabelText("To"), "2026-01-31");
    await waitFor(() => {
      const last = new URL(String(fn.mock.calls.at(-1)?.[0]), "http://app.test").searchParams;
      expect(last.get("target_type")).toBe("session");
      expect(new Date(last.get("from") ?? "").getDate()).toBe(1);
      expect(new Date(last.get("to") ?? "").getDate()).toBe(1);
      expect(new Date(last.get("to") ?? "").getMonth()).toBe(1);
    });
    await userEvent.click(screen.getByRole("button", { name: "Clear filters" }));
    expect(screen.getByLabelText("Target type")).toHaveValue("");
    expect(screen.getByLabelText("From")).toHaveValue("");
  });

  // 001-AC22: v0 filters are target type and date range; the actor filter arrives in phase 3 with
  // wider visibility. A new control must update this test on purpose.
  it("offers exactly the target type and date filters", async () => {
    mockApi({ "/api/v1/audit": { items: [] } });
    renderApp("/audit");
    const form = await screen.findByRole("form", { name: "Filter audit entries" });
    const controls = [...form.querySelectorAll("input, select")].map((el) => ({
      label: (el as HTMLInputElement).labels?.[0]?.textContent,
      type: el.tagName === "SELECT" ? "select" : (el as HTMLInputElement).type,
    }));
    expect(controls).toEqual([
      { label: "Target type", type: "select" },
      { label: "From", type: "date" },
      { label: "To", type: "date" },
    ]);
  });

  it("loads the next page with the cursor", async () => {
    const fn = mockApi({
      "/api/v1/audit": (url: URL) =>
        Response.json(
          url.searchParams.get("cursor") === "c2"
            ? { items: [entry(3)] }
            : { items: [entry(1), entry(2)], next_cursor: "c2" },
        ),
    });
    renderApp("/audit");
    await userEvent.click(await screen.findByRole("button", { name: "Load more" }));
    await waitFor(() =>
      expect(screen.getAllByRole("button", { name: /Show details/ })).toHaveLength(3),
    );
    expect(screen.queryByRole("button", { name: "Load more" })).not.toBeInTheDocument();
    expect(fn.mock.calls.some(([u]) => String(u).includes("cursor=c2"))).toBe(true);
  });

  it("reports a failed next page", async () => {
    mockApi({
      "/api/v1/audit": (url: URL) =>
        url.searchParams.get("cursor")
          ? new Response("", { status: 500 })
          : Response.json({ items: [entry(1)], next_cursor: "c2" }),
    });
    renderApp("/audit");
    await userEvent.click(await screen.findByRole("button", { name: "Load more" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("next page could not be loaded");
  });

  it("shows the empty state, and a filtered one with a way out", async () => {
    mockApi({ "/api/v1/audit": { items: [] } });
    renderApp("/audit");
    expect(await screen.findByText("No activity recorded")).toBeInTheDocument();
    await userEvent.selectOptions(screen.getByLabelText("Target type"), "user");
    expect(await screen.findByText("No entries match these filters")).toBeInTheDocument();
    const [, emptyStateClear] = screen.getAllByRole("button", { name: "Clear filters" });
    if (!emptyStateClear) throw new Error("the filtered empty state offers no way out");
    await userEvent.click(emptyStateClear);
    expect(await screen.findByText("No activity recorded")).toBeInTheDocument();
  });

  it("shows an error with a working retry", async () => {
    let fail = true;
    mockApi({
      "/api/v1/audit": () =>
        fail
          ? Response.json({ detail: "db down" }, { status: 500 })
          : Response.json({ items: [entry(1)] }),
    });
    renderApp("/audit");
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("could not be loaded");
    expect(alert).toHaveTextContent("db down");
    fail = false;
    await userEvent.click(within(alert).getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("table", { name: /Audit entries/ })).toBeInTheDocument();
  });
});

describe("actionTone", () => {
  it.each([
    ["user.created", "emerald"],
    ["user.login", "emerald"],
    ["watch.deleted", "rose"],
    ["session.logout", "rose"],
    ["watch.updated", "beacon"],
  ])("%s is %s", (action, tone) => {
    expect(actionTone(action)).toContain(tone);
  });
});

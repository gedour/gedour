import { EditorView } from "@codemirror/view";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { navigation } from "@/api/client";
import { mockApi } from "@/test/api";
import { renderApp } from "@/test/render";
import { preview, script } from "@/test/routing";
import { lastCall, pending, problem } from "@/test/watches";
import type { DryRun } from "@/types/api";

beforeEach(() => {
  vi.spyOn(navigation, "toLogin").mockImplementation(() => {});
});

function editor(): EditorView {
  const v = EditorView.findFromDOM(document.querySelector(".cm-content") as HTMLElement);
  if (!v) throw new Error("no editor");
  return v;
}

/** Replaces the editor's text as typing would. */
function type(text: string) {
  act(() => {
    const v = editor();
    v.dispatch({ changes: { from: 0, to: v.state.doc.length, insert: text } });
  });
}

const RUN: DryRun = {
  ok: true,
  outcome: "ok",
  logs: ["received release.published", "receiver answered 200"],
  result: '{"status":200}',
  fetches: [
    {
      method: "POST",
      url: "https://hooks.example.com/gedour",
      headers: { Authorization: "[redacted]" },
      body_size: 31,
    },
  ],
  duration_ms: 23,
};

describe("ScriptsPage", () => {
  it("lists scripts with their channels and update time", async () => {
    mockApi({
      "/api/v1/scripts": {
        items: [
          script({ channels: [{ id: "c1", name: "Runner", type: "script" }], size: 2048 }),
          script({ id: "s2", name: "Spare", channels: [] }),
        ],
      },
    });
    renderApp("/scripts");
    const rows = within(await screen.findByRole("list", { name: "Scripts" })).getAllByRole(
      "listitem",
    );
    expect(rows[0]).toHaveTextContent("Notify deployUsed by 1 channel · 2.0 KiB");
    expect(rows[1]).toHaveTextContent("No channel runs it");
    expect(within(rows[0] as HTMLElement).getByRole("link")).toHaveAttribute("href", "/scripts/s1");
  });

  // 001-AC24
  it("has loading, empty and error states", async () => {
    mockApi({ "/api/v1/scripts": pending });
    const first = renderApp("/scripts");
    expect(screen.getAllByTestId("script-skeleton")).toHaveLength(3);
    first.unmount();
    mockApi({ "/api/v1/scripts": { items: null } });
    const second = renderApp("/scripts");
    expect(await screen.findByRole("region", { name: "No scripts yet" })).toBeInTheDocument();
    second.unmount();
    mockApi({ "/api/v1/scripts": () => new Response("", { status: 500 }) });
    renderApp("/scripts");
    expect(await screen.findByText("Scripts could not be loaded.")).toBeInTheDocument();
  });
});

describe("ScriptFormPage", () => {
  // 001-AC18
  it("creates a script, shows a syntax error inline and dry-runs the editor's code", async () => {
    let saves = 0;
    const fetch = mockApi({
      "/api/v1/scripts": (_: URL, init?: RequestInit) => {
        if (init?.method !== "POST") return Response.json({ items: [] });
        saves++;
        if (saves === 1)
          return problem(400, "invalid script", [
            {
              location: "body.source",
              message: "Unexpected token } (line 2, column 1)",
              value: { line: 2, column: 1 },
            },
          ]);
        return Response.json(script({ id: "s9", name: "Hook" }), { status: 201 });
      },
      "/api/v1/scripts/s9": script({ id: "s9", name: "Hook" }),
      "/api/v1/scripts/dry-run": RUN,
      "/api/v1/rules/preview": { items: [preview(7, true)] },
    });
    const user = userEvent.setup();
    const { router } = renderApp("/scripts/new");
    await screen.findByRole("textbox", { name: "Source" });
    expect(editor().state.doc.toString()).toContain("function main(event, ctx)");
    await user.type(screen.getByLabelText("Name"), "Hook");
    type("function main() {\n}}");
    await user.click(screen.getByRole("button", { name: "Create script" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Unexpected token } (line 2");
    expect(screen.getByRole("textbox", { name: "Source" })).toHaveAttribute("aria-invalid", "true");
    // Editing clears the stale error.
    type("function main(e) { log(e.type) }");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();

    // The run sends the unsaved text with the picked sample.
    await screen.findByRole("option", { name: /GitHub CLI releases: v7 published/ });
    await user.selectOptions(screen.getByLabelText("Event"), "event:7");
    await user.click(screen.getByRole("button", { name: "Run with sample event" }));
    const results = await screen.findByTestId("script-results");
    expect(within(results).getByRole("region", { name: "Logs" })).toHaveTextContent(
      "received release.publishedreceiver answered 200",
    );
    expect(within(results).getByRole("region", { name: "Fetches" })).toHaveTextContent(
      "POSThttps://hooks.example.com/gedourAuthorization: [redacted] · 31 B body",
    );
    expect(results).toHaveTextContent("Returned in 23 ms");
    expect(lastCall(fetch, "/api/v1/scripts/dry-run", "POST")?.body).toEqual({
      source: "function main(e) { log(e.type) }",
      event_id: 7,
    });

    await user.click(screen.getByRole("button", { name: "Create script" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/scripts/s9"));
    expect(lastCall(fetch, "/api/v1/scripts", "POST")?.body).toEqual({
      name: "Hook",
      source: "function main(e) { log(e.type) }",
    });
  });

  it("shows a failed run, a syntax error from the run and a request error", async () => {
    let n = 0;
    mockApi({
      "/api/v1/scripts/s1": script(),
      "/api/v1/scripts/dry-run": () => {
        n++;
        if (n === 1)
          return Response.json({
            ok: false,
            outcome: "timeout",
            error: "stopped after 5s",
            logs: null,
            fetches: null,
            duration_ms: 5003,
          });
        if (n === 2)
          return problem(400, "invalid script", [
            { location: "body.source", message: "Unexpected end of input", value: null },
          ]);
        return problem(429, "too many dry runs: one per second");
      },
      "/api/v1/rules/preview": () => new Response("", { status: 500 }),
    });
    const user = userEvent.setup();
    renderApp("/scripts/s1");
    expect(await screen.findByRole("heading", { name: "Edit Notify deploy" })).toBeInTheDocument();
    expect(screen.getByText(/Pick an event and run/)).toBeInTheDocument();
    const run = screen.getByRole("button", { name: "Run with sample event" });
    await user.click(run);
    const results = await screen.findByTestId("script-results");
    expect(results).toHaveTextContent("Timed out");
    expect(within(results).getByRole("alert")).toHaveTextContent("stopped after 5s");
    expect(results).toHaveTextContent("No log line.");
    expect(results).toHaveTextContent("None.");
    await user.click(run);
    expect(await screen.findByText("Unexpected end of input")).toBeInTheDocument();
    await user.click(run);
    expect(await screen.findByText("too many dry runs: one per second")).toBeInTheDocument();
  });

  it("saves with Mod-S, and refuses to delete a script channels run", async () => {
    const fetch = mockApi({
      "/api/v1/scripts/s1": (_: URL, init?: RequestInit) => {
        if (init?.method === "PATCH") return Response.json(script({ size: 99 }));
        if (init?.method === "DELETE")
          return problem(409, "used by Runner", [
            { location: "channels", message: "runs this script", value: "Runner" },
          ]);
        return Response.json(script());
      },
    });
    const user = userEvent.setup();
    renderApp("/scripts/s1");
    await screen.findByRole("textbox", { name: "Source" });
    act(() => {
      editor().contentDOM.dispatchEvent(
        new KeyboardEvent("keydown", { key: "s", ctrlKey: true, bubbles: true, cancelable: true }),
      );
    });
    expect(await screen.findByText("Saved Notify deploy")).toBeInTheDocument();
    expect(lastCall(fetch, "/api/v1/scripts/s1", "PATCH")?.body).toMatchObject({
      name: "Notify deploy",
    });
    await user.click(screen.getByRole("button", { name: "Delete script" }));
    const dialog = screen.getByRole("dialog", { name: "Delete Notify deploy?" });
    await user.click(within(dialog).getByRole("button", { name: "Delete" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent(
      "These channels still run it:Runner",
    );
    await user.click(within(dialog).getByRole("button", { name: "Close" }));
  });

  it("deletes a script, and shows a missing one as not found", async () => {
    mockApi({
      "/api/v1/scripts/s1": (_: URL, init?: RequestInit) =>
        init?.method === "DELETE" ? new Response(null, { status: 204 }) : Response.json(script()),
      "/api/v1/scripts": { items: [] },
    });
    const user = userEvent.setup();
    const { router } = renderApp("/scripts/s1");
    await user.click(await screen.findByRole("button", { name: "Delete script" }));
    await user.click(
      within(await screen.findByRole("dialog", { name: "Delete Notify deploy?" })).getByRole(
        "button",
        {
          name: "Delete",
        },
      ),
    );
    await waitFor(() => expect(router.state.location.pathname).toBe("/scripts"));

    mockApi({ "/api/v1/scripts/nope": () => problem(404, "script not found") });
    renderApp("/scripts/nope");
    expect(await screen.findByText(/not found/i)).toBeInTheDocument();
  });

  it("shows loading and load errors", async () => {
    mockApi({ "/api/v1/scripts/s1": pending });
    const first = renderApp("/scripts/s1");
    expect(await screen.findByText("Loading…")).toBeInTheDocument();
    first.unmount();
    mockApi({ "/api/v1/scripts/s1": () => new Response("", { status: 500 }) });
    renderApp("/scripts/s1");
    expect(await screen.findByText("The script could not be loaded.")).toBeInTheDocument();
  });
});

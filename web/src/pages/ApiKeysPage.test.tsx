import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { mockApi } from "@/test/api";
import { renderApp } from "@/test/render";
import { lastCall, pending, problem } from "@/test/watches";
import type { ApiKey } from "@/types/api";

const KEY = `gdr_abcdefgh${"x".repeat(35)}`;
const PATH = "/api/v1/api-keys";

function apiKey(over: Partial<ApiKey> = {}): ApiKey {
  return {
    id: "k1",
    name: "laptop",
    scope: "read",
    prefix: "abcdefgh",
    created_at: "2026-11-01T00:00:00Z",
    expires_at: "2027-01-30T00:00:00Z",
    last_used_at: null,
    revoked_at: null,
    effective_until: "2026-12-01T00:00:00Z",
    status: "active",
    ...over,
  };
}

const clipboard = (writeText: unknown) =>
  Object.defineProperty(navigator, "clipboard", {
    value: writeText && { writeText },
    configurable: true,
  });

describe("ApiKeysPage", () => {
  it("is in the navigation", async () => {
    mockApi({ [PATH]: { items: [] } });
    renderApp("/inbox");
    await userEvent.click(await screen.findByRole("link", { name: "API keys" }));
    expect(await screen.findByRole("heading", { level: 1, name: "API keys" })).toBeInTheDocument();
  });

  // 007-AC32: name, scope, prefix, created, last used, effective end and status, and no revoke on a revoked key.
  it("lists the keys", async () => {
    mockApi({
      [PATH]: {
        items: [
          apiKey(),
          apiKey({
            id: "k2",
            name: "ci",
            scope: "manage",
            effective_until: null,
            last_used_at: "2026-11-01T00:00:00Z",
          }),
          apiKey({ id: "k3", name: "old", status: "revoked", revoked_at: "2026-11-02T00:00:00Z" }),
          apiKey({ id: "k4", name: "idle", status: "dormant" }),
          apiKey({ id: "k5", name: "held", status: "suspended" }),
        ],
      },
    });
    renderApp("/keys");
    await screen.findByText("laptop");
    const rows = within(screen.getByRole("table", { name: "API keys" })).getAllByRole("row");
    expect(rows[0]).toHaveTextContent(
      /Name.*Scope.*Prefix.*Created.*Last used.*Effective end.*Status/,
    );
    expect(rows[1]).toHaveTextContent("laptop");
    expect(rows[1]).toHaveTextContent("gdr_abcdefgh…");
    expect(rows[1]).toHaveTextContent("Never");
    expect(rows[1]).toHaveTextContent("active");
    expect(rows[2]).toHaveTextContent("manage");
    expect(rows[2]).toHaveTextContent("No end date");
    expect(within(rows[3] as HTMLElement).queryByRole("button", { name: "Revoke" })).toBeNull();
    expect(rows[3]).toHaveTextContent("—");
    expect(rows[4]).toHaveTextContent("Sign in to wake it");
    expect(rows[4]).toHaveTextContent("dormant");
    // 007-AC12: a suspended key still offers Revoke.
    expect(rows[5]).toHaveTextContent("Restored at the next sign-in");
    expect(rows[5]).toHaveTextContent("suspended");
    expect(
      within(rows[5] as HTMLElement).getByRole("button", { name: "Revoke" }),
    ).toBeInTheDocument();
  });

  // 007-AC32: loading, empty and error states.
  it("has loading, empty and error states", async () => {
    mockApi({ [PATH]: pending });
    const first = renderApp("/keys");
    expect(await screen.findAllByTestId("api-key-skeleton")).toHaveLength(3);
    first.unmount();

    mockApi({ [PATH]: { items: [] } });
    const second = renderApp("/keys");
    expect(await screen.findByRole("region", { name: "No API keys yet" })).toBeInTheDocument();
    second.unmount();

    let fail = true;
    mockApi({
      [PATH]: () =>
        fail ? new Response("", { status: 500 }) : Response.json({ items: [apiKey()] }),
    });
    renderApp("/keys");
    expect(await screen.findByRole("alert")).toHaveTextContent("API keys could not be loaded.");
    fail = false;
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("laptop")).toBeInTheDocument();
  });

  // 007-AC32: the key shows once in a native dialog Escape cannot dismiss, with a copy button and this instance's URL.
  it("creates a key and reveals it once", async () => {
    let items: ApiKey[] = [];
    const fn = mockApi({
      [PATH]: (_: URL, init?: RequestInit) => {
        if (init?.method !== "POST") return Response.json({ items });
        items = [apiKey()];
        return Response.json({ ...items[0], key: KEY }, { status: 201 });
      },
    });
    const writeText = vi.fn().mockResolvedValue(undefined);
    clipboard(writeText);
    renderApp("/keys");
    await userEvent.click(await screen.findByRole("button", { name: "New API key" }));
    const dialog = screen.getByRole("dialog", { name: "New API key" });
    expect(within(dialog).getByRole("radio", { name: "Read" })).toBeChecked();
    expect(within(dialog).getByLabelText("Duration")).toHaveValue("90");
    expect(within(dialog).queryByText(/silence alerts/)).toBeNull();
    // A screen reader hears the scope as the name and what it allows as the description.
    expect(within(dialog).getByRole("radio", { name: "Manage" })).toHaveAccessibleDescription(
      /creates and edits watches/,
    );
    await userEvent.type(within(dialog).getByLabelText("Name"), "laptop");
    await userEvent.click(within(dialog).getByRole("radio", { name: "Manage" }));
    expect(
      within(dialog).getByText(
        "A hijacked manage key can silence alerts by disabling a rule or pausing a watch, and route events to your existing channels: give it only to clients that confirm writes with a person.",
      ),
    ).toBeInTheDocument();
    await userEvent.selectOptions(within(dialog).getByLabelText("Duration"), "never");
    expect(within(dialog).getByText(/no end date\. It still stops/)).toBeInTheDocument();
    await userEvent.click(within(dialog).getByRole("button", { name: "Create key" }));

    const box = await within(dialog).findByTestId("api-key-reveal");
    expect(lastCall(fn, PATH, "POST")?.body).toEqual({
      name: "laptop",
      scope: "manage",
      duration: "never",
    });
    expect(within(box).getByLabelText("API key")).toHaveValue(KEY);
    expect(within(box).getByLabelText("Client setup")).toHaveValue(
      `claude mcp add --scope user --transport http gedour ${window.location.origin}/mcp --header "Authorization: Bearer ${KEY}"`,
    );
    await userEvent.click(within(box).getByRole("button", { name: "Copy API key" }));
    expect(writeText).toHaveBeenCalledWith(KEY);
    expect(await within(box).findByRole("button", { name: "Copied API key" })).toBeInTheDocument();
    const esc = new Event("cancel", { cancelable: true });
    dialog.dispatchEvent(esc);
    expect(esc.defaultPrevented).toBe(true);

    await userEvent.click(within(box).getByRole("button", { name: "I have copied it" }));
    await waitFor(() => expect(screen.queryByTestId("api-key-reveal")).toBeNull());
    expect(await screen.findByText("gdr_abcdefgh…")).toBeInTheDocument();
    expect(document.body.innerHTML).not.toContain(KEY);

    // Opening the dialog again starts blank: the key is gone from the page's state.
    await userEvent.click(screen.getByRole("button", { name: "New API key" }));
    expect(
      within(screen.getByRole("dialog", { name: "New API key" })).getByLabelText("Name"),
    ).toHaveValue("");
    expect(document.body.innerHTML).not.toContain(KEY);
  });

  // A response arriving after a cancel would store a key the user believes they never made.
  it("cannot be cancelled while the create is in flight", async () => {
    let answer: (r: Response) => void = () => {};
    const held = new Promise<Response>((resolve) => (answer = resolve));
    mockApi({
      [PATH]: (_: URL, init?: RequestInit) =>
        init?.method === "POST" ? held : Response.json({ items: [] }),
    });
    renderApp("/keys");
    await userEvent.click(await screen.findByRole("button", { name: "New API key" }));
    const dialog = screen.getByRole<HTMLDialogElement>("dialog");
    await userEvent.type(within(dialog).getByLabelText("Name"), "laptop");
    const cancel = within(dialog).getByRole("button", { name: "Cancel" });
    expect(cancel).toBeEnabled();
    const before = new Event("cancel", { cancelable: true });
    dialog.dispatchEvent(before);
    expect(before.defaultPrevented).toBe(false);
    await userEvent.click(within(dialog).getByRole("button", { name: "Create key" }));

    await waitFor(() => expect(cancel).toBeDisabled());
    const esc = new Event("cancel", { cancelable: true });
    dialog.dispatchEvent(esc);
    expect(esc.defaultPrevented).toBe(true);

    answer(Response.json({ ...apiKey(), key: KEY }, { status: 201 }));
    const box = await within(dialog).findByTestId("api-key-reveal");
    expect(within(box).getByLabelText("API key")).toHaveValue(KEY);
  });

  // Security: once the dialog closes, nothing in the client holds the key: not the page, not the mutation cache.
  it("leaves no key in the mutation cache after close", async () => {
    mockApi({
      [PATH]: (_: URL, init?: RequestInit) =>
        init?.method === "POST"
          ? Response.json({ ...apiKey(), key: KEY }, { status: 201 })
          : Response.json({ items: [] }),
    });
    const { client } = renderApp("/keys");
    await userEvent.click(await screen.findByRole("button", { name: "New API key" }));
    const dialog = screen.getByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText("Name"), "laptop");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create key" }));
    const box = await within(dialog).findByTestId("api-key-reveal");
    const cached = () =>
      JSON.stringify(
        client
          .getMutationCache()
          .getAll()
          .map((m) => m.state),
      );
    // The check itself sees the key while it is legitimately held.
    expect(cached()).toContain(KEY);

    await userEvent.click(within(box).getByRole("button", { name: "I have copied it" }));
    await waitFor(() => expect(client.getMutationCache().getAll()).toHaveLength(0));
    expect(cached()).not.toContain(KEY);
  });

  // No clipboard outside a secure context, or a browser refusal: the text gets selected instead.
  it.each([
    ["refused", vi.fn().mockRejectedValue(new Error("denied"))],
    ["absent", undefined],
  ])("selects the key when the clipboard is %s", async (_, writeText) => {
    mockApi({
      [PATH]: (_: URL, init?: RequestInit) =>
        init?.method === "POST"
          ? Response.json({ ...apiKey(), key: KEY }, { status: 201 })
          : Response.json({ items: [] }),
    });
    clipboard(writeText);
    renderApp("/keys");
    await userEvent.click(await screen.findByRole("button", { name: "New API key" }));
    const dialog = screen.getByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText("Name"), "laptop");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create key" }));
    const box = await within(dialog).findByTestId("api-key-reveal");
    await userEvent.click(within(box).getByRole("button", { name: "Copy Client setup" }));
    expect(await within(box).findByRole("alert")).toHaveTextContent("press Ctrl+C");
    const input = within(box).getByLabelText<HTMLInputElement>("Client setup");
    expect(input.selectionStart).toBe(0);
    expect(input.selectionEnd).toBe(input.value.length);
  });

  // 007-AC3: the server's refusal at the cap shows in the form, which stays open.
  it("shows field errors and the cap refusal", async () => {
    let answer = problem(400, "invalid API key", [
      { location: "body.name", message: "must be 1 to 64 characters" },
    ]);
    mockApi({
      [PATH]: (_: URL, init?: RequestInit) =>
        init?.method === "POST" ? answer : Response.json({ items: [] }),
    });
    renderApp("/keys");
    await userEvent.click(await screen.findByRole("button", { name: "New API key" }));
    const dialog = screen.getByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create key" }));
    expect(await within(dialog).findByText("must be 1 to 64 characters")).toBeInTheDocument();
    answer = problem(409, "you already have 10 active API keys: revoke one first");
    await userEvent.type(within(dialog).getByLabelText("Name"), "x");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create key" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("revoke one first");
    await userEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  // A problem on the scope or the duration shows next to it, not as an unplaced alert.
  it.each([
    ["scope", "must be one of read, write, manage"],
    ["duration", "must be 7, 30, 90, 365 or never"],
  ])("shows a %s error on its field", async (field, message) => {
    mockApi({
      [PATH]: (_: URL, init?: RequestInit) =>
        init?.method === "POST"
          ? problem(400, "invalid API key", [{ location: `body.${field}`, message }])
          : Response.json({ items: [] }),
    });
    renderApp("/keys");
    await userEvent.click(await screen.findByRole("button", { name: "New API key" }));
    const dialog = screen.getByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create key" }));
    expect(await within(dialog).findByText(message)).toBeInTheDocument();
    expect(within(dialog).queryByText(`${field}: ${message}`)).toBeNull();
  });

  // A lost answer may hide a committed key: the form says so and the list refetches, so the key can be revoked.
  it.each([
    ["a network drop", () => Promise.reject(new TypeError("Failed to fetch"))],
    ["a 500", () => problem(500, "internal error")],
  ])("reports %s and refreshes the list", async (_, fail) => {
    let items: ApiKey[] = [];
    mockApi({
      [PATH]: (__: URL, init?: RequestInit) => {
        if (init?.method !== "POST") return Response.json({ items });
        items = [apiKey()];
        return fail();
      },
    });
    renderApp("/keys");
    await userEvent.click(await screen.findByRole("button", { name: "New API key" }));
    const dialog = screen.getByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText("Name"), "laptop");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create key" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent(
      "The key may have been created: check the list before retrying",
    );
    await userEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(await screen.findByText("gdr_abcdefgh…")).toBeInTheDocument();
  });

  // A lost answer may hide a committed revoke: the row must not keep saying active.
  it.each([
    ["answers 500", () => problem(500, "internal error"), "internal error"],
    ["loses its answer", () => Promise.reject(new TypeError("Failed to fetch")), "Failed to fetch"],
  ])("shows a revoke that commits then %s", async (_, fail, message) => {
    let items = [apiKey()];
    mockApi({
      [PATH]: () => Response.json({ items }),
      [`${PATH}/k1`]: () => {
        items = [apiKey({ status: "revoked", revoked_at: "2026-11-02T00:00:00Z" })];
        return fail();
      },
    });
    renderApp("/keys");
    await userEvent.click(await screen.findByRole("button", { name: "Revoke" }));
    await userEvent.click(
      within(screen.getByRole("dialog")).getByRole("button", { name: "Revoke" }),
    );
    expect(await screen.findByText(message)).toBeInTheDocument();
    expect(await screen.findByText("revoked")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Revoke" })).toBeNull();
  });

  // A revoke left running behind a closed dialog would toast late and shut one opened since for another key.
  it("cannot be cancelled while the revoke is in flight", async () => {
    let answer: (r: Response) => void = () => {};
    const held = new Promise<Response>((resolve) => (answer = resolve));
    mockApi({
      [PATH]: { items: [apiKey()] },
      [`${PATH}/k1`]: () => held,
    });
    renderApp("/keys");
    await userEvent.click(await screen.findByRole("button", { name: "Revoke" }));
    const dialog = screen.getByRole("dialog");
    const cancel = within(dialog).getByRole("button", { name: "Cancel" });
    const before = new Event("cancel", { cancelable: true });
    dialog.dispatchEvent(before);
    expect(before.defaultPrevented).toBe(false);
    await userEvent.click(within(dialog).getByRole("button", { name: "Revoke" }));

    await waitFor(() => expect(cancel).toBeDisabled());
    const esc = new Event("cancel", { cancelable: true });
    dialog.dispatchEvent(esc);
    expect(esc.defaultPrevented).toBe(true);
    expect(dialog).toHaveAttribute("open");

    answer(new Response(null, { status: 204 }));
    expect(await screen.findByText("Revoked laptop")).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  // 007-AC32: revoking asks first.
  it("revokes after a confirmation", async () => {
    let items = [apiKey()];
    mockApi({
      [PATH]: () => Response.json({ items }),
      [`${PATH}/k1`]: () => {
        items = [apiKey({ status: "revoked", revoked_at: "2026-11-02T00:00:00Z" })];
        return new Response(null, { status: 204 });
      },
    });
    renderApp("/keys");
    await userEvent.click(await screen.findByRole("button", { name: "Revoke" }));
    let dialog = screen.getByRole("dialog", { name: "Revoke laptop?" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(items[0]?.status).toBe("active");

    await userEvent.click(screen.getByRole("button", { name: "Revoke" }));
    dialog = screen.getByRole("dialog", { name: "Revoke laptop?" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Revoke" }));
    expect(await screen.findByText("Revoked laptop")).toBeInTheDocument();
    expect(await screen.findByText("revoked")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Revoke" })).toBeNull();
    // The row's button is gone, so focus must not fall back to the body.
    await waitFor(() => expect(screen.getByRole("button", { name: "New API key" })).toHaveFocus());
  });

  it("toasts a failed revoke and keeps the key listed", async () => {
    mockApi({
      [PATH]: { items: [apiKey()] },
      [`${PATH}/k1`]: () => problem(500, "internal error"),
    });
    renderApp("/keys");
    await userEvent.click(await screen.findByRole("button", { name: "Revoke" }));
    const dialog = screen.getByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Revoke" }));
    expect(await screen.findByText("internal error")).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.getByText("active")).toBeInTheDocument();
    // The row's button is still there, so the focus stays with it.
    expect(screen.getByRole("button", { name: "New API key" })).not.toHaveFocus();
  });
});

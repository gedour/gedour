import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { mockApi } from "@/test/api";
import { renderApp } from "@/test/render";
import { credential, lastCall, pending, problem } from "@/test/watches";
import type { Credential } from "@/types/api";

const SECRET = "ghp_supersecretvalue1234";

describe("CredentialsPage", () => {
  it("is reachable from settings", async () => {
    mockApi({ "/api/v1/credentials": { items: [] } });
    renderApp("/settings");
    await userEvent.click(screen.getByRole("link", { name: /Credentials/ }));
    expect(screen.getByRole("heading", { level: 1, name: "Credentials" })).toBeInTheDocument();
  });

  // 001-AC6: only name, kind, username, last 4 and usage are shown.
  it("lists credentials without their secrets", async () => {
    mockApi({
      "/api/v1/credentials": {
        items: [
          credential({ used_by_count: 1 }),
          credential({
            id: "c2",
            name: "Registry",
            kind: "registry_basic",
            username: "robot",
            last4: "",
            used_by_count: 2,
          }),
        ],
      },
    });
    renderApp("/settings/credentials");
    await screen.findByText("GitHub bot");
    const table = screen.getByRole("table", { name: "Credentials" });
    const rows = within(table).getAllByRole("row");
    expect(rows[1]).toHaveTextContent("GitHub botGitHub token—••••cdef1 watch");
    expect(rows[2]).toHaveTextContent("RegistryRegistry loginrobot••••2 watches");
  });

  // 001-AC24: loading, empty and error states.
  it("has loading, empty and error states", async () => {
    mockApi({ "/api/v1/credentials": pending });
    const first = renderApp("/settings/credentials");
    expect(screen.getAllByTestId("credential-skeleton")).toHaveLength(3);
    first.unmount();

    mockApi({ "/api/v1/credentials": { items: [] } });
    const second = renderApp("/settings/credentials");
    expect(await screen.findByRole("region", { name: "No credentials yet" })).toBeInTheDocument();
    second.unmount();

    let fail = true;
    mockApi({
      "/api/v1/credentials": () =>
        fail ? new Response("", { status: 500 }) : Response.json({ items: [credential()] }),
    });
    renderApp("/settings/credentials");
    expect(await screen.findByRole("alert")).toHaveTextContent("Credentials could not be loaded.");
    fail = false;
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("GitHub bot")).toBeInTheDocument();
  });

  // 001-AC6: the secret is sent once, write-only, and never displayed after saving.
  it("creates a registry login and forgets the secret", async () => {
    let items: Credential[] = [];
    const fn = mockApi({
      "/api/v1/credentials": (_: URL, init?: RequestInit) => {
        if (init?.method !== "POST") return Response.json({ items });
        const body = JSON.parse(String(init.body)) as { name: string };
        if (body.name === "dup") {
          return problem(409, "a credential with this name exists", [
            { location: "body.name", message: "you already have a credential with this name" },
          ]);
        }
        items = [
          credential({ name: body.name, kind: "registry_basic", username: "robot", last4: "1234" }),
        ];
        return Response.json(items[0], { status: 201 });
      },
    });
    renderApp("/settings/credentials");
    await screen.findByText("No credentials yet");
    await userEvent.click(screen.getByRole("button", { name: "Add credential" }));
    const dialog = screen.getByRole("dialog", { name: "Add a credential" });
    await userEvent.type(within(dialog).getByLabelText("Name"), "dup");
    await userEvent.selectOptions(within(dialog).getByLabelText("Kind"), "registry_basic");
    await userEvent.type(within(dialog).getByLabelText("Username"), "robot");
    const secret = within(dialog).getByLabelText("Password");
    expect(secret).toHaveAttribute("type", "password");
    await userEvent.type(secret, SECRET);
    await userEvent.click(within(dialog).getByRole("button", { name: "Save credential" }));
    expect(
      await within(dialog).findByText("you already have a credential with this name"),
    ).toBeInTheDocument();

    await userEvent.clear(within(dialog).getByLabelText("Name"));
    await userEvent.type(within(dialog).getByLabelText("Name"), "Quay");
    await userEvent.click(within(dialog).getByRole("button", { name: "Save credential" }));
    expect(await screen.findByText("Quay")).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(lastCall(fn, "/api/v1/credentials", "POST")?.body).toEqual({
      name: "Quay",
      kind: "registry_basic",
      secret: SECRET,
      username: "robot",
    });
    expect(document.body.innerHTML).not.toContain(SECRET);
  });

  it("shows errors of fields the dialog hides at its top", async () => {
    mockApi({
      "/api/v1/credentials": (_: URL, init?: RequestInit) =>
        init?.method === "POST"
          ? problem(400, "invalid credential", [
              {
                location: "body.username",
                message: "only registry_basic credentials have a username",
              },
            ])
          : Response.json({ items: [] }),
    });
    renderApp("/settings/credentials");
    await userEvent.click(await screen.findByRole("button", { name: "Add credential" }));
    const dialog = screen.getByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText("Name"), "x");
    await userEvent.type(within(dialog).getByLabelText("Token"), SECRET);
    await userEvent.click(within(dialog).getByRole("button", { name: "Save credential" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent(
      "username: only registry_basic credentials have a username",
    );
  });

  // A failure with no field to blame (network drop, bare 500) must not leave the form silent.
  it.each([
    ["a network drop", () => Promise.reject(new TypeError("Failed to fetch")), "Failed to fetch"],
    ["a bare 500", () => new Response("", { status: 500 }), /500/],
  ])("says so on %s", async (_, answer, message) => {
    mockApi({
      "/api/v1/credentials": (__: URL, init?: RequestInit) =>
        init?.method === "POST" ? answer() : Response.json({ items: [] }),
    });
    renderApp("/settings/credentials");
    await userEvent.click(await screen.findByRole("button", { name: "Add credential" }));
    const dialog = screen.getByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText("Name"), "x");
    await userEvent.type(within(dialog).getByLabelText("Token"), SECRET);
    await userEvent.click(within(dialog).getByRole("button", { name: "Save credential" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent(message);
  });

  it("rotates a secret", async () => {
    const fn = mockApi({
      "/api/v1/credentials": { items: [credential()] },
      "/api/v1/credentials/c1/secret": (_: URL, init?: RequestInit) => {
        const { secret } = JSON.parse(String(init?.body)) as { secret: string };
        return secret.length < 8
          ? problem(400, "validation failed", [{ location: "body.secret", message: "too short" }])
          : Response.json(credential({ last4: "9999" }));
      },
    });
    renderApp("/settings/credentials");
    await userEvent.click(await screen.findByRole("button", { name: "Rotate secret" }));
    const dialog = screen.getByRole("dialog", { name: "Rotate GitHub bot" });
    await userEvent.type(within(dialog).getByLabelText("New secret"), "short");
    await userEvent.click(within(dialog).getByRole("button", { name: "Replace secret" }));
    expect(await within(dialog).findByText("too short")).toBeInTheDocument();
    await userEvent.type(within(dialog).getByLabelText("New secret"), "-but-now-long");
    await userEvent.click(within(dialog).getByRole("button", { name: "Replace secret" }));
    expect(await screen.findByText("Rotated the secret of GitHub bot")).toBeInTheDocument();
    expect(lastCall(fn, "/api/v1/credentials/c1/secret", "PUT")?.body).toEqual({
      secret: "short-but-now-long",
    });
  });

  // 001-AC6: deleting a credential in use is refused and the UI names the watches.
  it("explains a 409 on delete, and deletes an unused one", async () => {
    let inUse = true;
    mockApi({
      "/api/v1/credentials": { items: [credential()] },
      "/api/v1/credentials/c1": () =>
        inUse
          ? problem(409, "used by Go releases: detach it from these watches first", [
              { location: "watches", message: "uses this credential", value: "Go releases" },
            ])
          : new Response(null, { status: 204 }),
    });
    renderApp("/settings/credentials");
    await userEvent.click(await screen.findByRole("button", { name: "Delete" }));
    let dialog = screen.getByRole("dialog", { name: "Delete GitHub bot?" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    const alert = await within(dialog).findByRole("alert");
    expect(alert).toHaveTextContent("This credential is still used by:Go releases");
    await userEvent.click(within(dialog).getByRole("button", { name: "Close" }));

    inUse = false;
    await userEvent.click(screen.getByRole("button", { name: "Delete" }));
    dialog = screen.getByRole("dialog", { name: "Delete GitHub bot?" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    expect(await screen.findByText("Deleted GitHub bot")).toBeInTheDocument();
  });

  it("shows other delete failures in the dialog", async () => {
    mockApi({
      "/api/v1/credentials": { items: [credential()] },
      "/api/v1/credentials/c1": () => problem(500, "boom"),
    });
    renderApp("/settings/credentials");
    await userEvent.click(await screen.findByRole("button", { name: "Delete" }));
    const dialog = screen.getByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("boom");
    await userEvent.keyboard("{Escape}");
  });
});

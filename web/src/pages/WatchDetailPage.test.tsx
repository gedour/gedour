import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { navigation } from "@/api/client";
import { mockApi } from "@/test/api";
import { renderApp } from "@/test/render";
import { credential, lastCall, pending, problem, watch } from "@/test/watches";
import type { Share } from "@/types/api";

beforeEach(() => {
  vi.spyOn(navigation, "toLogin").mockImplementation(() => {});
});

const bob: Share = {
  user_id: "u-bob",
  email: "bob@example.test",
  display_name: "Bob",
  permission: "ro",
  created_at: "2026-11-01T00:00:00Z",
};

describe("WatchDetailPage", () => {
  it("shows the config, schedule and attached credential to the owner", async () => {
    mockApi({
      "/api/v1/watches/w1": watch({ credential_id: "c1", schedule: "0 9 * * 1-5" }),
      "/api/v1/watches/w1/shares": { items: [] },
      "/api/v1/credentials": { items: [credential()] },
    });
    renderApp("/watches/w1");
    expect(
      await screen.findByRole("heading", { level: 1, name: "Go releases" }),
    ).toBeInTheDocument();
    const config = screen.getByRole("region", { name: "Configuration" });
    expect(config).toHaveTextContent("Repositorygolang/go");
    expect(config).toHaveTextContent("Include pre-releasesNo");
    expect(config).toHaveTextContent("At 09:00 on weekdays");
    expect(await within(config).findByText("GitHub bot")).toBeInTheDocument();
    expect(screen.getByText("Not shared with anyone.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Edit" })).toHaveAttribute("href", "/watches/w1/edit");
  });

  // 001-AC4: pause and resume from the detail view.
  it("pauses through the switch", async () => {
    const fn = mockApi({
      "/api/v1/watches/w1": watch(),
      "/api/v1/watches/w1/shares": { items: [] },
      "/api/v1/watches/w1/pause": watch({ paused: true }),
    });
    renderApp("/watches/w1");
    const sw = await screen.findByRole("switch", { name: "Paused" });
    expect(sw).toHaveAttribute("aria-checked", "false");
    await userEvent.click(sw);
    await waitFor(() => expect(sw).toHaveAttribute("aria-checked", "true"));
    expect(lastCall(fn, "/api/v1/watches/w1/pause", "POST")).toBeDefined();
    expect(screen.getByRole("status")).toHaveTextContent("Paused Go releases");
  });

  // 001-AC5: the owner shares by email, changes the permission and removes the share.
  it("lets the owner manage shares", async () => {
    let shares: Share[] = [];
    const fn = mockApi({
      "/api/v1/watches/w1": watch(),
      "/api/v1/watches/w1/shares": (_: URL, init?: RequestInit) => {
        if (init?.method !== "POST") return Response.json({ items: shares });
        const body = JSON.parse(String(init.body)) as { email: string };
        if (body.email === "ghost@example.test") {
          return problem(404, "no user with that email has signed in yet", [
            { location: "body.email", message: "no user with that email has signed in yet" },
          ]);
        }
        shares = [bob];
        return Response.json(bob, { status: 201 });
      },
      "/api/v1/watches/w1/shares/u-bob": (_: URL, init?: RequestInit) => {
        if (init?.method === "DELETE") {
          shares = [];
          return new Response(null, { status: 204 });
        }
        shares = [{ ...bob, permission: "rw" }];
        return Response.json(shares[0]);
      },
    });
    renderApp("/watches/w1");
    const email = await screen.findByLabelText("Email");
    await userEvent.type(email, "ghost@example.test");
    await userEvent.click(screen.getByRole("button", { name: "Share" }));
    expect(
      await screen.findByText("no user with that email has signed in yet"),
    ).toBeInTheDocument();

    await userEvent.clear(email);
    await userEvent.type(email, "bob@example.test");
    await userEvent.selectOptions(screen.getByLabelText("Access"), "rw");
    await userEvent.click(screen.getByRole("button", { name: "Share" }));
    const list = await screen.findByRole("list", { name: "Shared with" });
    expect(list).toHaveTextContent("Bob");
    expect(lastCall(fn, "/api/v1/watches/w1/shares", "POST")?.body).toEqual({
      email: "bob@example.test",
      permission: "rw",
    });

    await userEvent.selectOptions(screen.getByLabelText("Access of bob@example.test"), "rw");
    await waitFor(() =>
      expect(lastCall(fn, "/api/v1/watches/w1/shares/u-bob", "PUT")?.body).toEqual({
        permission: "rw",
      }),
    );
    await userEvent.click(screen.getByRole("button", { name: "Remove bob@example.test" }));
    expect(await screen.findByText("Not shared with anyone.")).toBeInTheDocument();
  });

  // 001-AC5: a read-only sharee sees the watch and their permission, no edit controls.
  it("shows a read-only sharee no edit controls", async () => {
    mockApi({
      "/api/v1/watches/w1": watch({ permission: "ro", owner_name: "Alice", credential_id: "c1" }),
      "/api/v1/watches/w1/shares": { items: [bob] },
    });
    renderApp("/watches/w1");
    await screen.findByRole("heading", { level: 1, name: "Go releases" });
    expect(screen.getByText("Shared by Alice (ro)")).toBeInTheDocument();
    expect(screen.getByText(/Alice shared this watch with you, read-only/)).toBeInTheDocument();
    expect(screen.getByText("Attached by the owner")).toBeInTheDocument();
    for (const name of ["Edit", "Paused", "Delete watch", "Share"]) {
      expect(screen.queryByRole("button", { name })).not.toBeInTheDocument();
      expect(screen.queryByRole("link", { name })).not.toBeInTheDocument();
    }
    expect(await screen.findByRole("list", { name: "Shared with" })).toHaveTextContent("read-only");
  });

  // 001-AC4: deletion asks for confirmation in a native dialog first.
  it("deletes after confirmation", async () => {
    const fn = mockApi({
      "/api/v1/watches/w1": (_: URL, init?: RequestInit) =>
        init?.method === "DELETE" ? new Response(null, { status: 204 }) : Response.json(watch()),
      "/api/v1/watches/w1/shares": { items: [] },
      "/api/v1/watches": { items: [] },
    });
    renderApp("/watches/w1");
    await userEvent.click(await screen.findByRole("button", { name: "Delete watch" }));
    const dialog = screen.getByRole("dialog", { name: "Delete Go releases?" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(lastCall(fn, "/api/v1/watches/w1", "DELETE")).toBeUndefined();

    await userEvent.click(screen.getByRole("button", { name: "Delete watch" }));
    await userEvent.click(
      within(screen.getByRole("dialog")).getByRole("button", { name: "Delete" }),
    );
    expect(await screen.findByRole("heading", { level: 1, name: "Watches" })).toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("Deleted Go releases");
  });

  it("reports a failed delete and a failed pause", async () => {
    mockApi({
      "/api/v1/watches/w1": (_: URL, init?: RequestInit) =>
        init?.method === "DELETE"
          ? problem(403, "only the watch owner can do this")
          : Response.json(watch()),
      "/api/v1/watches/w1/shares": { items: [] },
      "/api/v1/watches/w1/pause": () => problem(403, "nope"),
    });
    renderApp("/watches/w1");
    await userEvent.click(await screen.findByRole("switch", { name: "Paused" }));
    expect(await screen.findByText(/Could not change the paused state: nope/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Delete watch" }));
    await userEvent.click(
      within(screen.getByRole("dialog")).getByRole("button", { name: "Delete" }),
    );
    expect(await screen.findByText(/Could not delete the watch/)).toBeInTheDocument();
    await userEvent.click(screen.getAllByRole("button", { name: "Dismiss" })[0] as HTMLElement);
  });

  // 001-AC5: a watch that is not shared reads as not found.
  it("shows the not-found view on 404, an error with retry otherwise, and a loading state", async () => {
    mockApi({ "/api/v1/watches/w1": () => problem(404, "watch not found") });
    const first = renderApp("/watches/w1");
    expect(await screen.findByRole("heading", { name: "Page not found" })).toBeInTheDocument();
    first.unmount();

    mockApi({ "/api/v1/watches/w1": () => new Response("", { status: 500 }) });
    const second = renderApp("/watches/w1");
    expect(await screen.findByRole("alert")).toHaveTextContent("The watch could not be loaded.");
    second.unmount();

    mockApi({ "/api/v1/watches/w1": pending });
    renderApp("/watches/w1");
    expect(screen.getByLabelText("Loading watch")).toBeInTheDocument();
  });
});

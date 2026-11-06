import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { mockApi } from "@/test/api";
import { renderApp } from "@/test/render";
import { credential, lastCall, pending, problem, watch } from "@/test/watches";

describe("WatchDetailPage", () => {
  it("shows the config, schedule and attached credential to the owner", async () => {
    mockApi({
      "/api/v1/watches/w1": watch({ credential_id: "c1", schedule: "0 9 * * 1-5" }),
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
    // 009-AC10: no share section, badge or form.
    expect(screen.queryByRole("region", { name: "Sharing" })).not.toBeInTheDocument();
    expect(screen.queryByRole("form", { name: "Share" })).not.toBeInTheDocument();
    expect(document.body).not.toHaveTextContent(/shar/i);
    expect(screen.getByRole("link", { name: "Edit" })).toHaveAttribute("href", "/watches/w1/edit");
  });

  // 001-AC4: pause and resume from the detail view.
  it("pauses through the switch", async () => {
    const fn = mockApi({
      "/api/v1/watches/w1": watch(),
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

  // 001-AC4: deletion asks for confirmation in a native dialog first.
  it("deletes after confirmation", async () => {
    const fn = mockApi({
      "/api/v1/watches/w1": (_: URL, init?: RequestInit) =>
        init?.method === "DELETE" ? new Response(null, { status: 204 }) : Response.json(watch()),
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
      within(await screen.findByRole("dialog")).getByRole("button", { name: "Delete" }),
    );
    expect(await screen.findByRole("heading", { level: 1, name: "Watches" })).toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("Deleted Go releases");
  });

  it("warns what the delete removes and holds the dialog open while it runs", async () => {
    mockApi({
      "/api/v1/watches/w1": (_: URL, init?: RequestInit) =>
        init?.method === "DELETE" ? pending() : Response.json(watch()),
    });
    renderApp("/watches/w1");
    await userEvent.click(await screen.findByRole("button", { name: "Delete watch" }));
    const dialog = screen.getByRole("dialog");
    expect(dialog).toHaveTextContent(/its events and every rule on it/);
    expect(dialog).toHaveTextContent(/Every inbox item and delivery for those events/);
    // 009-AC10: the delete dialog names no share.
    expect(dialog).not.toHaveTextContent(/shar/i);
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    expect(within(dialog).getByRole("button", { name: "Cancel" })).toBeDisabled();
    expect(fireEvent(dialog, new Event("cancel", { cancelable: true }))).toBe(false);
    (dialog as HTMLDialogElement).close();
    expect(screen.getByRole("dialog")).toHaveAttribute("open");
  });

  it("reports a failed delete and a failed pause", async () => {
    mockApi({
      "/api/v1/watches/w1": (_: URL, init?: RequestInit) =>
        init?.method === "DELETE" ? problem(409, "the watch changed") : Response.json(watch()),
      "/api/v1/watches/w1/pause": () => problem(403, "nope"),
    });
    renderApp("/watches/w1");
    await userEvent.click(await screen.findByRole("switch", { name: "Paused" }));
    expect(await screen.findByText(/Could not change the paused state: nope/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Delete watch" }));
    await userEvent.click(
      within(await screen.findByRole("dialog")).getByRole("button", { name: "Delete" }),
    );
    expect(await screen.findByText(/Could not delete the watch/)).toBeInTheDocument();
    await userEvent.click(screen.getAllByRole("button", { name: "Dismiss" })[0] as HTMLElement);
  });

  // 009-AC6: another user's watch answers 404 and reads as not found, like a missing one.
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

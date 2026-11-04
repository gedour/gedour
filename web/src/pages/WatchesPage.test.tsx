import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { navigation } from "@/api/client";
import { mockApi } from "@/test/api";
import { renderApp } from "@/test/render";
import { lastCall, pending, watch } from "@/test/watches";

beforeEach(() => {
  vi.spyOn(navigation, "toLogin").mockImplementation(() => {});
});

describe("WatchesPage", () => {
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
            permission: "ro",
            owner_name: "Bob",
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
    expect(rows[1]).toHaveTextContent("Shared by Bob (ro)");
    expect(rows[0]).not.toHaveTextContent("Shared by");
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

  it("sends search, kind and ownership filters, and clears them", async () => {
    const fn = mockApi({
      "/api/v1/watches": (url: URL) =>
        Response.json({ items: url.searchParams.size > 1 ? [] : [watch()] }),
    });
    renderApp("/watches");
    await screen.findByText("Go releases");
    await userEvent.type(screen.getByLabelText("Search"), "go");
    await userEvent.selectOptions(screen.getByLabelText("Kind"), "github_pr");
    const mine = screen.getByRole("button", { name: "Shared with me" });
    await userEvent.click(mine);
    expect(mine).toHaveAttribute("aria-pressed", "true");
    await waitFor(() => {
      const q = lastCall(fn, "/api/v1/watches")?.url.searchParams;
      expect(q?.get("q")).toBe("go");
      expect(q?.get("kind")).toBe("github_pr");
      expect(q?.get("shared")).toBe("with_me");
    });
    expect(await screen.findByText("No watches match these filters")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Clear filters" }));
    expect(screen.getByLabelText("Search")).toHaveValue("");
    expect(await screen.findByText("Go releases")).toBeInTheDocument();
  });
});

import { screen } from "@testing-library/react";
import { expect, it, vi } from "vitest";

import { renderApp } from "@/test/render";

// Stands for the 404 of a rollout: the dynamic import rejects.
vi.mock("@/pages/ScriptFormPage", () => {
  throw new TypeError("Failed to fetch dynamically imported module");
});

it("a lost chunk shows the message inside the shell, on both script routes", async () => {
  for (const path of ["/scripts/new", "/scripts/0191"]) {
    const { unmount } = renderApp(path);
    expect(await screen.findByRole("alert")).toHaveTextContent("This page could not be loaded.");
    expect(screen.getByRole("button", { name: "Reload" })).toBeInTheDocument();
    expect(screen.getByRole("navigation", { name: "Main" })).toBeInTheDocument();
    expect(screen.queryByText(/Unexpected Application Error/)).toBeNull();
    unmount();
  }
});

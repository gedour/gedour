import { render, screen } from "@testing-library/react";
import { expect, it, vi } from "vitest";

import { ScriptFormRoute } from "@/pages/ScriptFormRoute";

vi.mock("@/pages/ScriptFormPage", () => ({ ScriptFormPage: () => <p>Script form</p> }));

it("a loaded chunk forgets the reloads it took", async () => {
  sessionStorage.setItem("gedour.chunkReloads", JSON.stringify({ count: 3, at: Date.now() }));
  render(<ScriptFormRoute />);
  expect(await screen.findByText("Script form")).toBeInTheDocument();
  expect(sessionStorage.getItem("gedour.chunkReloads")).toBeNull();
});

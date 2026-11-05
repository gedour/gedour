import { screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { mockApi, FILTER_REFERENCE } from "@/test/api";
import { renderApp } from "@/test/render";

describe("FilterReference", () => {
  // The editor shows the server's allowlist, not a hand-written copy.
  it("renders what the server allows", async () => {
    mockApi({ "/api/v1/rules/preview": { items: [] } });
    renderApp("/rules/new");
    const ref = await screen.findByRole("region", { name: "Filter reference" });
    expect(ref).toHaveTextContent("Operators: == contains matches");
    expect(ref).toHaveTextContent("Functions: len lower semver_gt");
    expect(ref).toHaveTextContent("Over an array: any count");
    expect(ref).toHaveTextContent(FILTER_REFERENCE.rules[0] as string);
    expect(ref).toHaveTextContent(
      "At most 2000 characters, 200 expression nodes and a cost of 4,000,000 units",
    );
  });

  it("says when the reference is unavailable", async () => {
    mockApi({
      "/api/v1/rules/preview": { items: [] },
      "/api/v1/filters/reference": () => new Response("", { status: 500 }),
    });
    renderApp("/rules/new");
    expect(
      await screen.findByText(/unavailable; saving still checks the filter/),
    ).toBeInTheDocument();
  });
});

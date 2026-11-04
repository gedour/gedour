import { render, screen } from "@testing-library/react";
import { expect, it } from "vitest";

import { EmptyState } from "@/components/EmptyState";

it("labels each empty state by its own title", () => {
  render(
    <>
      <EmptyState title="First" body="a" />
      <EmptyState title="Second" body="b" />
    </>,
  );
  const first = screen.getByRole("region", { name: "First" });
  const second = screen.getByRole("region", { name: "Second" });
  expect(first.getAttribute("aria-labelledby")).not.toBe(second.getAttribute("aria-labelledby"));
});

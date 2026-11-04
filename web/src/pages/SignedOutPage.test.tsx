import { screen } from "@testing-library/react";
import { expect, it } from "vitest";

import { mockApi } from "@/test/api";
import { renderApp } from "@/test/render";

// Landing after sign-out: no API call (a 401 would bounce back to login), a way to sign in again.
it("shows the signed-out page without calling the API", async () => {
  const fn = mockApi();
  renderApp("/auth/signed-out");
  expect(screen.getByRole("heading", { level: 1, name: "You are signed out" })).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "Sign in again" })).toHaveAttribute(
    "href",
    "/auth/login?return_to=%2F",
  );
  expect(screen.queryByRole("complementary", { name: "Sidebar" })).not.toBeInTheDocument();
  await new Promise((r) => setTimeout(r, 20));
  expect(fn).not.toHaveBeenCalled();
});

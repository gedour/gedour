import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createMemoryRouter, Link, Outlet, RouterProvider } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { navigation } from "@/api/client";
import { RouteError } from "@/components/RouteError";
import { chunkLoaded, ChunkLoadError } from "@/lib/chunks";

const KEY = "gedour.chunkReloads";

function Throws({ error }: { error: Error }): never {
  throw error;
}

function renderRoute(error: Error) {
  const router = createMemoryRouter(
    [
      {
        path: "/",
        element: (
          <>
            <Link to="/other">Other</Link>
            <Outlet />
          </>
        ),
        children: [
          { path: "lazy", element: <Throws error={error} />, errorElement: <RouteError /> },
          { path: "other", element: <p>Other page</p> },
        ],
      },
    ],
    { initialEntries: ["/lazy"] },
  );
  render(<RouterProvider router={router} />);
}

const alert = () => screen.getByRole("alert");
const failed = () =>
  new ChunkLoadError(new TypeError("Failed to fetch dynamically imported module"));

describe("RouteError", () => {
  let reload: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    sessionStorage.clear();
    vi.useFakeTimers({ shouldAdvanceTime: true });
    reload = vi.spyOn(navigation, "reload").mockImplementation(() => {});
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("shows a readable message, not the import error", () => {
    renderRoute(failed());
    expect(alert()).toHaveTextContent("This page could not be loaded.");
    expect(alert()).toHaveTextContent("Reload to try again.");
    expect(alert()).not.toHaveTextContent("dynamically imported");
  });

  it.each([
    [null, 2000, "1"],
    ["1", 4000, "2"],
    ["2", 8000, "3"],
    ["3", 16000, "4"],
  ])("after %s reloads waits %d ms and counts it", (used, delay, next) => {
    if (used) sessionStorage.setItem(KEY, used);
    renderRoute(failed());
    act(() => void vi.advanceTimersByTime(delay - 1));
    expect(reload).not.toHaveBeenCalled();
    expect(sessionStorage.getItem(KEY)).toBe(used);
    act(() => void vi.advanceTimersByTime(1));
    expect(reload).toHaveBeenCalledOnce();
    expect(sessionStorage.getItem(KEY)).toBe(next);
  });

  it("stops at the limit, and the button still reloads", async () => {
    sessionStorage.setItem(KEY, "4");
    renderRoute(failed());
    act(() => void vi.advanceTimersByTime(600_000));
    expect(reload).not.toHaveBeenCalled();
    await userEvent
      .setup({ advanceTimers: vi.advanceTimersByTime })
      .click(screen.getByRole("button", { name: "Reload" }));
    expect(reload).toHaveBeenCalledOnce();
    expect(sessionStorage.getItem(KEY)).toBe("4");
  });

  it("does not reload once the user left the page", async () => {
    renderRoute(failed());
    await userEvent
      .setup({ advanceTimers: vi.advanceTimersByTime })
      .click(screen.getByRole("link", { name: "Other" }));
    expect(screen.getByText("Other page")).toBeInTheDocument();
    act(() => void vi.advanceTimersByTime(600_000));
    expect(reload).not.toHaveBeenCalled();
    expect(sessionStorage.getItem(KEY)).toBeNull();
  });

  it("does not reload while offline", () => {
    vi.spyOn(navigator, "onLine", "get").mockReturnValue(false);
    renderRoute(failed());
    act(() => void vi.advanceTimersByTime(600_000));
    expect(reload).not.toHaveBeenCalled();
    expect(sessionStorage.getItem(KEY)).toBeNull();
  });

  it("does not reload without a counter", () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new DOMException("denied", "SecurityError");
    });
    renderRoute(failed());
    act(() => void vi.advanceTimersByTime(600_000));
    expect(reload).not.toHaveBeenCalled();
    expect(alert()).toBeInTheDocument();
  });

  it("does not reload when the counter cannot be written", () => {
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new DOMException("full", "QuotaExceededError");
    });
    renderRoute(failed());
    act(() => void vi.advanceTimersByTime(600_000));
    expect(reload).not.toHaveBeenCalled();
  });

  it("leaves other errors to the router", () => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    renderRoute(new Error("a render bug"));
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByText("a render bug")).toBeInTheDocument();
    act(() => void vi.advanceTimersByTime(600_000));
    expect(reload).not.toHaveBeenCalled();
  });

  it("forgets the counter when the chunk loads", () => {
    sessionStorage.setItem(KEY, "3");
    chunkLoaded();
    expect(sessionStorage.getItem(KEY)).toBeNull();
    vi.spyOn(Storage.prototype, "removeItem").mockImplementation(() => {
      throw new DOMException("denied", "SecurityError");
    });
    expect(chunkLoaded).not.toThrow();
  });
});

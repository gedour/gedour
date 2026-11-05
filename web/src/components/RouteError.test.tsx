import { act, fireEvent, render, screen } from "@testing-library/react";
import { createMemoryRouter, Link, Outlet, RouterProvider } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { auth, navigation } from "@/api/client";
import { RouteError } from "@/components/RouteError";
import { chunkLoaded, ChunkLoadError } from "@/lib/chunks";

const KEY = "gedour.chunkReloads";
const MINUTE = 60_000;
const FORGET_AFTER = 5 * MINUTE;

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
const advance = (ms: number) => act(() => vi.advanceTimersByTimeAsync(ms));
const click = async (el: HTMLElement) => {
  fireEvent.click(el);
  await advance(0);
};
const store = (count: number, age = 0) =>
  sessionStorage.setItem(KEY, JSON.stringify({ count, at: Date.now() - age }));
const stored = () => JSON.parse(sessionStorage.getItem(KEY) ?? "{}").count as number | undefined;

describe("RouteError", () => {
  let reload: ReturnType<typeof vi.spyOn>;
  let probe: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    sessionStorage.clear();
    vi.useFakeTimers();
    reload = vi.spyOn(navigation, "reload").mockImplementation(() => {});
    probe = vi.fn(async () => new Response("ok"));
    vi.stubGlobal("fetch", probe);
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it("shows a readable message, not the import error", () => {
    renderRoute(failed());
    expect(alert()).toHaveTextContent("This page could not be loaded.");
    expect(alert()).toHaveTextContent("Reload to try again.");
    expect(alert()).not.toHaveTextContent("dynamically imported");
  });

  it.each([
    [0, 2000],
    [1, 4000],
    [2, 8000],
    [3, 16000],
  ])("after %d reloads waits %d ms, probes, then counts it", async (used, delay) => {
    if (used) store(used);
    renderRoute(failed());
    await advance(delay - 1);
    expect(probe).not.toHaveBeenCalled();
    expect(reload).not.toHaveBeenCalled();
    await advance(1);
    expect(probe).toHaveBeenCalledExactlyOnceWith(
      "/healthz",
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
    expect(reload).toHaveBeenCalledOnce();
    expect(stored()).toBe(used + 1);
  });

  it.each([
    ["answers 502", async () => new Response("bad gateway", { status: 502 })],
    [
      "does not answer",
      async () => {
        throw new TypeError("Failed to fetch");
      },
    ],
  ])("a server that %s costs no try, and it recovers by itself", async (_, down) => {
    store(1);
    const before = sessionStorage.getItem(KEY);
    probe.mockImplementation(down);
    renderRoute(failed());
    await advance(4000);
    await advance(8000);
    expect(probe).toHaveBeenCalledTimes(2);
    expect(reload).not.toHaveBeenCalled();
    expect(sessionStorage.getItem(KEY)).toBe(before);
    expect(alert()).toBeInTheDocument();

    probe.mockImplementation(async () => new Response("ok"));
    await advance(16000);
    expect(reload).toHaveBeenCalledOnce();
    expect(stored()).toBe(2);
  });

  it("caps the wait between probes", async () => {
    store(3);
    probe.mockImplementation(async () => new Response("", { status: 503 }));
    renderRoute(failed());
    await advance(16000);
    expect(probe).toHaveBeenCalledTimes(1);
    await advance(29_999);
    expect(probe).toHaveBeenCalledTimes(1);
    await advance(1);
    expect(probe).toHaveBeenCalledTimes(2);
    await advance(30_000);
    expect(probe).toHaveBeenCalledTimes(3);
  });

  it("a 401 from the probe leaves the auth state alone", async () => {
    probe.mockImplementation(async () => new Response("", { status: 401 }));
    renderRoute(failed());
    await advance(2000);
    expect(probe).toHaveBeenCalledOnce();
    expect(auth.signedOut()).toBe(false);
    expect(reload).not.toHaveBeenCalled();
  });

  it("stops at the limit for good, and the button still reloads", async () => {
    store(4);
    renderRoute(failed());
    await advance(60 * MINUTE);
    expect(probe).not.toHaveBeenCalled();
    expect(reload).not.toHaveBeenCalled();
    await click(screen.getByRole("button", { name: "Reload" }));
    expect(reload).toHaveBeenCalledOnce();
    expect(stored()).toBe(4);
  });

  it("keeps the limit up to the time a series is forgotten", async () => {
    store(4, FORGET_AFTER);
    renderRoute(failed());
    await advance(MINUTE);
    expect(reload).not.toHaveBeenCalled();
  });

  it("starts over after a spent series is forgotten", async () => {
    store(4, FORGET_AFTER + 1);
    renderRoute(failed());
    await advance(1999);
    expect(reload).not.toHaveBeenCalled();
    await advance(1);
    expect(reload).toHaveBeenCalledOnce();
    expect(stored()).toBe(1);
  });

  it.each(["3", "null", "{", '{"count":4}'])("treats the stored value %s as no try", async (v) => {
    sessionStorage.setItem(KEY, v);
    renderRoute(failed());
    await advance(2000);
    expect(reload).toHaveBeenCalledOnce();
    expect(stored()).toBe(1);
  });

  it("does not reload once the user left the page", async () => {
    renderRoute(failed());
    await click(screen.getByRole("link", { name: "Other" }));
    expect(screen.getByText("Other page")).toBeInTheDocument();
    await advance(10 * MINUTE);
    expect(probe).not.toHaveBeenCalled();
    expect(reload).not.toHaveBeenCalled();
    expect(sessionStorage.getItem(KEY)).toBeNull();
  });

  it("does not reload when the user leaves during the probe", async () => {
    let answer: (r: Response) => void = () => {};
    probe.mockImplementation(() => new Promise((resolve) => (answer = resolve)));
    renderRoute(failed());
    await advance(2000);
    expect(probe).toHaveBeenCalledOnce();
    await click(screen.getByRole("link", { name: "Other" }));
    answer(new Response("ok"));
    await advance(0);
    expect(reload).not.toHaveBeenCalled();
    expect(sessionStorage.getItem(KEY)).toBeNull();
  });

  it("does not reload without a counter", async () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new DOMException("denied", "SecurityError");
    });
    renderRoute(failed());
    await advance(10 * MINUTE);
    expect(reload).not.toHaveBeenCalled();
    expect(alert()).toBeInTheDocument();
  });

  it("does not reload when the counter cannot be written", async () => {
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new DOMException("full", "QuotaExceededError");
    });
    renderRoute(failed());
    await advance(10 * MINUTE);
    expect(reload).not.toHaveBeenCalled();
  });

  it("leaves other errors to the router", async () => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    renderRoute(new Error("a render bug"));
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByText("a render bug")).toBeInTheDocument();
    await advance(10 * MINUTE);
    expect(reload).not.toHaveBeenCalled();
  });

  it("survives blocked storage when the chunk loads", () => {
    vi.spyOn(Storage.prototype, "removeItem").mockImplementation(() => {
      throw new DOMException("denied", "SecurityError");
    });
    expect(chunkLoaded).not.toThrow();
  });
});

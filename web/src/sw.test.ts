import { describe, expect, it, vi } from "vitest";

import SOURCE from "../public/sw.js?raw";

const ORIGIN = "https://gedour.test";

type Listener = (event: unknown) => void;

/** Runs public/sw.js against a stubbed worker scope and returns its listeners and stubs. */
function loadWorker(windows: object[] = []) {
  const listeners: Record<string, Listener> = {};
  const scope = {
    location: new URL(ORIGIN + "/sw.js"),
    addEventListener: (type: string, fn: Listener) => (listeners[type] = fn),
    skipWaiting: vi.fn(),
    registration: { showNotification: vi.fn().mockResolvedValue(undefined) },
    clients: {
      matchAll: vi.fn().mockResolvedValue(windows),
      openWindow: vi.fn().mockResolvedValue(null),
    },
  };
  new Function("self", SOURCE)(scope);
  /** Dispatches an extendable event and waits for what it handed to waitUntil. */
  const dispatch = async (type: string, fields: object) => {
    let waited: Promise<unknown> | undefined;
    listeners[type]?.({ ...fields, waitUntil: (p: Promise<unknown>) => (waited = p) });
    return waited;
  };
  return { scope, dispatch };
}

const pushData = (v: unknown) => ({
  json: () => (typeof v === "string" ? JSON.parse(v) : v),
});

/** An open gedour tab, as clients.matchAll gives it. */
const tab = (focus = vi.fn().mockResolvedValue(undefined), path = "/channels") => ({
  url: ORIGIN + path,
  focus,
  postMessage: vi.fn(),
});

describe("service worker", () => {
  // An updated worker takes over at once instead of waiting for every tab to close.
  it("activates an update without waiting", async () => {
    const { scope, dispatch } = loadWorker();
    await dispatch("install", {});
    expect(scope.skipWaiting).toHaveBeenCalled();
  });

  // 010-AC13: the pushed message becomes a notification, shown inside waitUntil.
  it("shows the pushed message", async () => {
    const { scope, dispatch } = loadWorker();
    await dispatch("push", {
      data: pushData({
        title: "cli/cli: v2",
        body: "Notes",
        url: "/inbox?event=4",
        tag: "gedour-event-4",
        silent: true,
      }),
    });
    expect(scope.registration.showNotification).toHaveBeenCalledWith("cli/cli: v2", {
      body: "Notes",
      tag: "gedour-event-4",
      silent: true,
      icon: "/icon-192.png",
      data: { url: "/inbox?event=4" },
    });
  });

  it("shows a default notification for an empty or broken payload", async () => {
    for (const data of [null, pushData("{not json")]) {
      const { scope, dispatch } = loadWorker();
      await dispatch("push", { data });
      expect(scope.registration.showNotification).toHaveBeenCalledWith("gedour", {
        body: "",
        tag: undefined,
        silent: false,
        icon: "/icon-192.png",
        data: { url: "/inbox" },
      });
    }
  });

  // 010-AC13: a click closes the notification, focuses the open tab and asks it to route there.
  it("routes the open tab in place on a click", async () => {
    const first = tab();
    const other = tab();
    const { scope, dispatch } = loadWorker([first, other]);
    const close = vi.fn();
    await dispatch("notificationclick", {
      notification: { close, data: { url: "/inbox?event=4" } },
    });
    expect(close).toHaveBeenCalled();
    expect(scope.clients.matchAll).toHaveBeenCalledWith({
      type: "window",
      includeUncontrolled: true,
    });
    expect(first.focus).toHaveBeenCalled();
    expect(first.postMessage).toHaveBeenCalledWith({ url: `${ORIGIN}/inbox?event=4` });
    expect(other.postMessage).not.toHaveBeenCalled();
    expect(scope.clients.openWindow).not.toHaveBeenCalled();
  });

  // 010-AC13: with no open tab, a new window opens; a foreign URL never leaves the origin.
  it("opens a window when no tab is open, on this origin only", async () => {
    for (const [url, want] of [
      ["/inbox?event=7", `${ORIGIN}/inbox?event=7`],
      ["https://evil.test/x", `${ORIGIN}/inbox`],
      ["//evil.test/x", `${ORIGIN}/inbox`],
      [undefined, `${ORIGIN}/inbox`],
    ] as const) {
      const { scope, dispatch } = loadWorker();
      await dispatch("notificationclick", { notification: { close: vi.fn(), data: { url } } });
      expect(scope.clients.openWindow).toHaveBeenCalledWith(want);
    }
    const t = tab();
    const { dispatch } = loadWorker([t]);
    await dispatch("notificationclick", {
      notification: { close: vi.fn(), data: { url: "https://evil.test/x" } },
    });
    expect(t.postMessage).toHaveBeenCalledWith({ url: `${ORIGIN}/inbox` });
  });

  // A tab showing the API, /mcp or the worker script runs no SPA: skip it, or open a window.
  it("skips tabs the app does not run in", async () => {
    const off = ["/api/v1/me", "/mcp", "/mcp/x", "/sw.js"].map((p) => tab(undefined, p));
    const app = tab(undefined, "/mcpx");
    const { dispatch } = loadWorker([...off, app]);
    await dispatch("notificationclick", {
      notification: { close: vi.fn(), data: { url: "/inbox" } },
    });
    for (const t of off) expect(t.postMessage).not.toHaveBeenCalled();
    expect(app.postMessage).toHaveBeenCalledWith({ url: `${ORIGIN}/inbox` });
    const only = loadWorker(off);
    await only.dispatch("notificationclick", {
      notification: { close: vi.fn(), data: { url: "/inbox" } },
    });
    expect(only.scope.clients.openWindow).toHaveBeenCalledWith(`${ORIGIN}/inbox`);
  });

  it("still routes the tab when focusing it is refused", async () => {
    const t = tab(vi.fn().mockRejectedValue(new Error("not allowed")));
    const { scope, dispatch } = loadWorker([t]);
    await dispatch("notificationclick", {
      notification: { close: vi.fn(), data: { url: "/inbox" } },
    });
    expect(t.postMessage).toHaveBeenCalledWith({ url: `${ORIGIN}/inbox` });
    expect(scope.clients.openWindow).not.toHaveBeenCalled();
  });
});

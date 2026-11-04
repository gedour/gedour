import { act, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { ME, mockApi } from "@/test/api";
import { renderApp } from "@/test/render";
import { SESSION_CHECK_MS } from "@/lib/inbox";
import { FakeEventSource, inboxItem } from "@/test/routing";

afterEach(() => {
  FakeEventSource.instances = [];
});

const unauthorized = () => Response.json({ status: 401 }, { status: 401 });
const gate = (name: string | RegExp = "Sign in to continue") =>
  screen.findByRole("heading", { level: 1, name });
const sidebar = () => screen.queryByRole("complementary", { name: "Sidebar" });
const calledPaths = (fn: ReturnType<typeof mockApi>) =>
  fn.mock.calls.map(([u]) => new URL(String(u), "http://app.test").pathname);
const settle = () => new Promise((r) => setTimeout(r, 30));
const ERROR_TEXT = /Could not|HTTP 401|failed/i;

/** Records every alert, toast or error text added to the page from now on, however briefly. */
function watchErrors() {
  const seen: string[] = [];
  const check = (el: Element) => {
    if (
      el.matches("[role=alert], [role=status][aria-live] *") ||
      ERROR_TEXT.test(el.textContent ?? "")
    )
      seen.push(el.outerHTML.slice(0, 160));
  };
  const observer = new MutationObserver((records) => {
    for (const r of records) {
      if (r.type === "characterData" && ERROR_TEXT.test(r.target.textContent ?? ""))
        seen.push(r.target.textContent ?? "");
      for (const n of r.addedNodes) {
        if (n instanceof Element) [n, ...n.querySelectorAll("*")].forEach(check);
        else if (ERROR_TEXT.test(n.textContent ?? "")) seen.push(n.textContent ?? "");
      }
    }
  });
  observer.observe(document.body, { childList: true, subtree: true, characterData: true });
  return { seen, stop: () => observer.disconnect() };
}

describe("AuthGate at boot", () => {
  // 002-AC1: no session, any URL: only the gate renders and only /api/v1/me is called.
  it.each(["/", "/inbox", "/watches/w1", "/settings/credentials", "/nowhere"])(
    "shows only the gate at %s",
    async (path) => {
      const fn = mockApi({ "/api/v1/me": unauthorized });
      renderApp(path, null);
      expect(await gate()).toBeInTheDocument();
      expect(await gate()).not.toHaveFocus();
      expect(sidebar()).not.toBeInTheDocument();
      expect(screen.queryByRole("banner")).not.toBeInTheDocument();
      await settle();
      expect(new Set(calledPaths(fn))).toEqual(new Set(["/api/v1/me"]));
    },
  );

  it("shows a neutral loading state, then the shell, once the session is confirmed", async () => {
    let answer: (r: Response) => void = () => {};
    mockApi({ "/api/v1/me": () => new Promise<Response>((r) => (answer = r)) });
    renderApp("/inbox", null);
    expect(screen.getByRole("status")).toHaveTextContent("Loading");
    expect(sidebar()).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { level: 1 })).not.toBeInTheDocument();
    act(() => answer(Response.json(ME)));
    expect(await screen.findByRole("complementary", { name: "Sidebar" })).toBeInTheDocument();
  });

  // 002-AC7: dev single-user mode answers /api/v1/me at once; the gate never shows.
  it("renders the app straight away in dev mode", async () => {
    mockApi({ "/api/v1/me": { ...ME, dev_mode: true } });
    renderApp("/inbox", null);
    expect(await screen.findByRole("heading", { level: 1, name: "Inbox" })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: /sign in/i })).not.toBeInTheDocument();
  });

  it("says the server is unreachable and retries the session check", async () => {
    let up = false;
    mockApi({ "/api/v1/me": () => (up ? Response.json(ME) : new Response("", { status: 502 })) });
    renderApp("/inbox", null);
    expect(await gate("gedour is unreachable")).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Sign in" })).not.toBeInTheDocument();
    up = true;
    await userEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByRole("heading", { level: 1, name: "Inbox" })).toBeInTheDocument();
  });
});

describe("SignInGate", () => {
  // 002-AC2: "Sign in" returns to the URL first requested, without the gate's own reason.
  it.each([
    ["/inbox", "/inbox"],
    ["/", "/"],
    ["/watches/w1?tab=events", "/watches/w1?tab=events"],
    ["/audit?auth=failed&from=x", "/audit?from=x"],
    ["/rules?auth=unavailable", "/rules"],
    ["/audit?from=a;b&to=x", "/audit?from=a;b&to=x"],
    ["/audit?from=%zz", "/audit?from=%zz"],
    ["/watches/w1?tab=events#details", "/watches/w1?tab=events#details"],
    ["/watches/w1?auth=failed&tab=events#details", "/watches/w1?tab=events#details"],
    ["/inbox?%61uth=failed", "/inbox"],
    ["/inbox?a%75th=nope&x=%zz", "/inbox?x=%zz"],
    ["/inbox?%61uth=a&x=1&auth=b&a%75th&AUTH=c", "/inbox?x=1&AUTH=c"],
    ["/audit?from=x&?auth=customer-data", "/audit?from=x&?auth=customer-data"],
  ])("at %s signs in with return_to %s", async (path, returnTo) => {
    mockApi({ "/api/v1/me": unauthorized });
    renderApp(path, null);
    await gate(/./);
    expect(screen.getByRole("link", { name: /^(Sign in|Try again)$/ })).toHaveAttribute(
      "href",
      `/auth/login?return_to=${encodeURIComponent(returnTo)}`,
    );
  });

  // 002-AC5 / 002-AC6: the fixed reason picks the message; anything else is the plain gate.
  it.each([
    ["unavailable", "Sign-in service unavailable", "Try again"],
    ["failed", "Sign-in failed, try again", "Try again"],
    ["<script>", "Sign in to continue", "Sign in"],
    ["constructor", "Sign in to continue", "Sign in"],
    ["toString", "Sign in to continue", "Sign in"],
    ["__proto__", "Sign in to continue", "Sign in"],
    ["hasOwnProperty", "Sign in to continue", "Sign in"],
    ["", "Sign in to continue", "Sign in"],
  ])("shows the message for auth=%j", async (reason, title, action) => {
    mockApi({ "/api/v1/me": unauthorized });
    renderApp(`/inbox?auth=${encodeURIComponent(reason)}`, null);
    expect(await gate(title)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: action })).toHaveAttribute(
      "href",
      "/auth/login?return_to=%2Finbox",
    );
    expect(document.body).not.toHaveTextContent("<script>");
  });

  // An encoded key is the same reason to the reader, and it is stripped from the return target too.
  it("reads an encoded reason key", async () => {
    mockApi({ "/api/v1/me": unauthorized });
    renderApp("/inbox?%61uth=unavailable&q=1", null);
    expect(await gate("Sign-in service unavailable")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Try again" })).toHaveAttribute(
      "href",
      "/auth/login?return_to=%2Finbox%3Fq%3D1",
    );
  });
});

describe("AuthGate mid-session", () => {
  // 002-AC3: a read answering 401 swaps the app for the gate, with no error view on the way.
  it("replaces the app on a 401 from a page's query", async () => {
    mockApi({ "/api/v1/audit": unauthorized });
    const errors = watchErrors();
    renderApp("/audit");
    expect(await gate()).toBeInTheDocument();
    // 002-AC8: focus leaves the vanished app for the gate.
    expect(await gate()).toHaveFocus();
    await settle();
    errors.stop();
    expect(errors.seen).toEqual([]);
    expect(sidebar()).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Sign in" })).toHaveAttribute(
      "href",
      "/auth/login?return_to=%2Faudit",
    );
  });

  // 002-AC3: a mutation answering 401 does the same, and its error toast never shows.
  it("replaces the app on a 401 from a mutation, without a toast", async () => {
    mockApi({
      "/api/v1/inbox": { items: [inboxItem(1)] },
      "/api/v1/inbox/read": unauthorized,
    });
    renderApp("/inbox");
    const item = await screen.findByRole("button", { name: /v1\.0\.0 published/ });
    const errors = watchErrors();
    await userEvent.click(item);
    expect(await gate()).toBeInTheDocument();
    expect(await gate()).toHaveFocus();
    await settle();
    errors.stop();
    expect(errors.seen).toEqual([]);
    expect(sidebar()).not.toBeInTheDocument();
  });

  // 002-AC4: the stream's own signed_out event.
  it("replaces the app when the stream says signed_out", async () => {
    vi.stubGlobal("EventSource", FakeEventSource);
    mockApi();
    renderApp("/watches");
    await screen.findByRole("heading", { level: 1, name: "Watches" });
    act(() => FakeEventSource.last().emit("signed_out", {}));
    expect(await gate()).toBeInTheDocument();
    expect(FakeEventSource.last().readyState).toBe(FakeEventSource.CLOSED);
  });

  // 002-AC4: a stream error rechecks the session; a 401 there signs out, a 200 keeps the app.
  it.each([
    [401, true],
    [200, false],
  ])("on a stream error with /api/v1/me answering %i, gate shown: %s", async (status, gated) => {
    vi.stubGlobal("EventSource", FakeEventSource);
    let meStatus = 200;
    const fn = mockApi({
      "/api/v1/me": () => (meStatus === 200 ? Response.json(ME) : unauthorized()),
    });
    renderApp("/watches");
    await screen.findByRole("heading", { level: 1, name: "Watches" });
    await waitFor(() => expect(calledPaths(fn)).toContain("/api/v1/me"));
    const before = calledPaths(fn).filter((p) => p === "/api/v1/me").length;
    meStatus = status;
    act(() => FakeEventSource.last().fail(false));
    await waitFor(() =>
      expect(calledPaths(fn).filter((p) => p === "/api/v1/me").length).toBe(before + 1),
    );
    await settle();
    if (gated) {
      expect(await gate()).toBeInTheDocument();
    } else {
      expect(screen.getByRole("heading", { level: 1, name: "Watches" })).toBeInTheDocument();
      expect(sidebar()).toBeInTheDocument();
    }
  });

  // 002-AC4: native retries stay CONNECTING; a session that ends after the first drop is still
  // caught by a later error, once the 30 s throttle has passed.
  it("rechecks the session on later CONNECTING errors, throttled", async () => {
    vi.stubGlobal("EventSource", FakeEventSource);
    const realNow = Date.now.bind(Date);
    let offset = 0;
    vi.spyOn(Date, "now").mockImplementation(() => realNow() + offset);
    let meStatus = 200;
    const fn = mockApi({
      "/api/v1/me": () => (meStatus === 200 ? Response.json(ME) : unauthorized()),
    });
    renderApp("/watches");
    await screen.findByRole("heading", { level: 1, name: "Watches" });
    const meCalls = () => calledPaths(fn).filter((p) => p === "/api/v1/me").length;
    await waitFor(() => expect(meCalls()).toBe(1));
    const es = FakeEventSource.last();
    act(() => es.fail(false));
    await waitFor(() => expect(meCalls()).toBe(2));

    meStatus = 401;
    for (let i = 0; i < 5; i++) act(() => es.fail(false));
    await settle();
    expect(meCalls()).toBe(2);
    expect(screen.getByRole("heading", { level: 1, name: "Watches" })).toBeInTheDocument();

    offset = SESSION_CHECK_MS;
    act(() => es.fail(false));
    expect(await gate()).toHaveFocus();
    expect(meCalls()).toBe(3);
    expect(FakeEventSource.instances).toHaveLength(1);
  });

  // A failed recheck that is not a 401 (server restarting) keeps the live session's app.
  it("keeps the app when the session recheck fails with a 5xx", async () => {
    vi.stubGlobal("EventSource", FakeEventSource);
    let down = false;
    mockApi({
      "/api/v1/me": () => (down ? new Response("", { status: 503 }) : Response.json(ME)),
    });
    renderApp("/watches");
    await screen.findByRole("heading", { level: 1, name: "Watches" });
    down = true;
    act(() => FakeEventSource.last().fail(false));
    await settle();
    expect(screen.getByRole("heading", { level: 1, name: "Watches" })).toBeInTheDocument();
    expect(
      screen.queryByRole("heading", { name: "gedour is unreachable" }),
    ).not.toBeInTheDocument();
  });
});

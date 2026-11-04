import { beforeEach, describe, expect, it, vi } from "vitest";

import {
  ApiError,
  CSRF_PROBLEM_TITLE,
  getMe,
  getVersion,
  listAudit,
  LOGIN_REDIRECT_KEY,
  logout,
  navigation,
  request,
  retryQuery,
} from "@/api/client";
import { ME, mockApi } from "@/test/api";

const sentHeaders = (fn: ReturnType<typeof mockApi>, call = 0) =>
  new Headers(fn.mock.calls[call]?.[1]?.headers);

describe("api client", () => {
  let toLogin: ReturnType<typeof vi.spyOn>;
  beforeEach(() => {
    toLogin = vi.spyOn(navigation, "toLogin").mockImplementation(() => {});
  });

  it("returns the decoded body", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(Response.json({ version: "v1" })));
    await expect(getVersion()).resolves.toEqual({ version: "v1" });
    expect(fetch).toHaveBeenCalledWith("/api/v1/version", expect.anything());
  });

  it("rejects on a non-2xx status with the problem detail", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(Response.json({ detail: "boom" }, { status: 503 })),
    );
    const err = await getVersion().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(503);
    expect((err as ApiError).message).toContain("HTTP 503: boom");
  });

  it("keeps the bare status when the error body is not JSON", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("<html>", { status: 502 })));
    await expect(getVersion()).rejects.toThrow(/HTTP 502$/);
  });

  // 001-AC3: mutations echo the CSRF token from /api/v1/me; reads never send it.
  it("sends the CSRF token on mutations only", async () => {
    const fn = mockApi({
      "/auth/logout": { redirect: "https://idp.test/logout?x=1" },
      "/api/v1/audit": { items: [] },
    });
    await getMe();
    expect(sentHeaders(fn, 0).has("X-CSRF-Token")).toBe(false);
    await expect(logout()).resolves.toBe("https://idp.test/logout?x=1");
    expect(fn.mock.calls[1]?.[0]).toBe("/auth/logout");
    expect(fn.mock.calls[1]?.[1]?.method).toBe("POST");
    expect(sentHeaders(fn, 1).get("X-CSRF-Token")).toBe(ME.csrf_token);
    await listAudit({}, "");
    expect(sentHeaders(fn, 2).has("X-CSRF-Token")).toBe(false);
  });

  const csrf403 = () => Response.json({ title: CSRF_PROBLEM_TITLE, status: 403 }, { status: 403 });

  // 001-AC3: a stale CSRF token is refreshed from /api/v1/me once, and the mutation retried once.
  it("retries a CSRF 403 once with a fresh token", async () => {
    let token = "old";
    const fn = mockApi({
      "/api/v1/me": () => Response.json({ ...ME, csrf_token: token }),
      "/auth/logout": (_: URL, init?: RequestInit) =>
        new Headers(init?.headers).get("X-CSRF-Token") === "new"
          ? Response.json({ redirect: "/auth/signed-out" })
          : csrf403(),
    });
    await getMe();
    token = "new";
    await expect(logout()).resolves.toBe("/auth/signed-out");
    const paths = fn.mock.calls.map(([u]) => u);
    expect(paths).toEqual(["/api/v1/me", "/auth/logout", "/api/v1/me", "/auth/logout"]);
  });

  it.each([
    ["fails", 500, false],
    ["is a 401", 401, true],
  ])("reports the original call when the token refresh %s", async (_, status, redirects) => {
    const toLogin = vi.mocked(navigation.toLogin);
    mockApi({
      "/api/v1/me": () => Response.json({}, { status }),
      "/auth/logout": csrf403,
    });
    const err = await logout().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(403);
    expect((err as ApiError).message).toMatch(
      /^\/auth\/logout: HTTP 403: CSRF token refresh failed/,
    );
    expect((err as ApiError).message).toContain(`HTTP ${status}`);
    expect(toLogin).toHaveBeenCalledTimes(redirects ? 1 : 0);
  });

  it("surfaces a second CSRF 403 instead of retrying again", async () => {
    const fn = mockApi({ "/auth/logout": csrf403 });
    const err = await logout().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(403);
    expect(fn.mock.calls.filter(([u]) => u === "/auth/logout")).toHaveLength(2);
  });

  it.each([
    [
      "a non-CSRF problem",
      () => Response.json({ title: "Forbidden", status: 403 }, { status: 403 }),
    ],
    ["a non-JSON body", () => new Response("nope", { status: 403 })],
  ])("does not retry a 403 with %s", async (_, answer) => {
    const fn = mockApi({ "/auth/logout": answer });
    await expect(logout()).rejects.toThrow("HTTP 403");
    expect(fn.mock.calls.filter(([u]) => u === "/auth/logout")).toHaveLength(1);
  });

  it("does not retry a 403 on a read", async () => {
    const fn = mockApi({ "/api/v1/audit": csrf403 });
    await expect(listAudit({}, "")).rejects.toThrow("HTTP 403");
    expect(fn).toHaveBeenCalledTimes(1);
  });

  it.each([
    ["a string", "x", 2],
    ["FormData", new FormData(), 2],
    ["URLSearchParams", new URLSearchParams("a=1"), 2],
    ["a stream", new ReadableStream(), 1],
  ])("replays %s body only if it can", async (_, body, attempts) => {
    const fn = mockApi({ "/api/v1/thing": csrf403 });
    await expect(request("/api/v1/thing", { method: "POST", body })).rejects.toThrow("HTTP 403");
    expect(fn.mock.calls.filter(([u]) => u === "/api/v1/thing")).toHaveLength(attempts);
  });

  // 001-AC1: a 401 sends the browser to the login page with the current location.
  it("redirects to login on 401", async () => {
    window.history.replaceState(null, "", "/audit?x=1");
    mockApi({ "/api/v1/me": () => Response.json({}, { status: 401 }) });
    await expect(getMe()).rejects.toThrow("HTTP 401");
    expect(toLogin).toHaveBeenCalledWith("/audit?x=1");
  });

  it("builds the audit query from set filters only", async () => {
    const fn = mockApi({ "/api/v1/audit": { items: [] } });
    await listAudit({ target_type: "user", from: "", to: "2026-01-02T00:00:00.000Z" }, "c1", 10);
    const url = new URL(String(fn.mock.calls[0]?.[0]), "http://app.test");
    expect(Object.fromEntries(url.searchParams)).toEqual({
      limit: "10",
      target_type: "user",
      to: "2026-01-02T00:00:00.000Z",
      cursor: "c1",
    });
  });
});

describe("navigation.toLogin", () => {
  it("redirects once, then holds off to avoid a login loop", () => {
    const assign = vi.fn();
    vi.stubGlobal("location", { ...window.location, assign });
    navigation.toLogin("/inbox");
    expect(assign).toHaveBeenCalledWith("/auth/login?return_to=%2Finbox");
    navigation.toLogin("/inbox");
    expect(assign).toHaveBeenCalledTimes(1);

    localStorage.setItem(LOGIN_REDIRECT_KEY, String(Date.now() - 60_000));
    navigation.toLogin("/a b");
    expect(assign).toHaveBeenLastCalledWith("/auth/login?return_to=%2Fa%20b");
  });

  it("re-arms after a successful sign-in", async () => {
    const assign = vi.fn();
    vi.stubGlobal("location", { ...window.location, assign });
    navigation.toLogin("/");
    mockApi();
    await getMe();
    navigation.toLogin("/");
    expect(assign).toHaveBeenCalledTimes(2);
  });
});

describe("retryQuery", () => {
  it("never retries a 4xx, retries other failures up to 3 times", () => {
    expect(retryQuery(0, new ApiError(404, "x"))).toBe(false);
    expect(retryQuery(0, new ApiError(403, "x"))).toBe(false);
    expect(retryQuery(0, new ApiError(500, "x"))).toBe(true);
    expect(retryQuery(2, new TypeError("network"))).toBe(true);
    expect(retryQuery(3, new ApiError(503, "x"))).toBe(false);
  });
});

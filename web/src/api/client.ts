import { readStorage, writeStorage } from "@/lib/utils";
import type { AuditFilters, AuditPage, Me, VersionInfo } from "@/types/api";

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
  }
}

// Set by getMe; every mutation echoes it so the server can tell our pages from a forged form.
let csrfToken = "";

export const LOGIN_REDIRECT_KEY = "gedour.login.redirectedAt";
const LOGIN_LOOP_MS = 15_000;

// An object, not a bare function, so tests can spy on the redirect.
export const navigation = {
  toLogin(returnTo: string) {
    // A cookie the browser refuses (Secure over plain http) would bounce us off the provider forever.
    const last = Number(readStorage(LOGIN_REDIRECT_KEY) ?? 0);
    if (Date.now() - last < LOGIN_LOOP_MS) return;
    writeStorage(LOGIN_REDIRECT_KEY, String(Date.now()));
    window.location.assign(`/auth/login?return_to=${encodeURIComponent(returnTo)}`);
  },
  go(url: string) {
    window.location.assign(url);
  },
};

const SAFE = new Set(["GET", "HEAD", "OPTIONS"]);

// Title of the server's CSRF problem (auth.CSRFProblemTitle); any other 403 is final.
export const CSRF_PROBLEM_TITLE = "CSRF token invalid";

// A stream body is consumed by the first attempt; only these can be sent again.
function replayable(body: RequestInit["body"]): boolean {
  return (
    body === undefined ||
    body === null ||
    typeof body === "string" ||
    body instanceof FormData ||
    body instanceof URLSearchParams
  );
}

async function isCSRFProblem(res: Response): Promise<boolean> {
  try {
    const body = (await res.clone().json()) as { title?: string };
    return body.title === CSRF_PROBLEM_TITLE;
  } catch {
    return false;
  }
}

export async function request(
  path: string,
  init: RequestInit = {},
  retried = false,
): Promise<Response> {
  const method = (init.method ?? "GET").toUpperCase();
  const headers = new Headers(init.headers);
  headers.set("Accept", "application/json");
  if (!SAFE.has(method) && csrfToken) headers.set("X-CSRF-Token", csrfToken);
  const res = await fetch(path, { ...init, method, headers });
  if (
    res.status === 403 &&
    !SAFE.has(method) &&
    !retried &&
    replayable(init.body) &&
    (await isCSRFProblem(res))
  ) {
    // The token may be stale (session replaced in another tab): refresh it once, then retry once.
    try {
      await getMe();
    } catch (err) {
      // getMe already sent a 401 to the login page; the caller still learns which call failed.
      const why = err instanceof Error ? err.message : String(err);
      throw new ApiError(
        res.status,
        `${path}: HTTP ${res.status}: CSRF token refresh failed (${why})`,
      );
    }
    return request(path, init, true);
  }
  if (res.status === 401) {
    // 001-AC1: a lost session sends the browser to the provider, then back here.
    navigation.toLogin(window.location.pathname + window.location.search);
  }
  if (!res.ok) {
    let detail = `HTTP ${res.status}`;
    try {
      const body = (await res.json()) as { detail?: string };
      if (body.detail) detail = `${detail}: ${body.detail}`;
    } catch {
      // not JSON: keep the status alone
    }
    throw new ApiError(res.status, `${path}: ${detail}`);
  }
  return res;
}

async function getJSON<T>(path: string): Promise<T> {
  return (await (await request(path)).json()) as T;
}

export function getVersion(): Promise<VersionInfo> {
  return getJSON<VersionInfo>("/api/v1/version");
}

export async function getMe(): Promise<Me> {
  const me = await getJSON<Me>("/api/v1/me");
  csrfToken = me.csrf_token;
  // Signed in: the next 401 is a fresh expiry, not a loop.
  writeStorage(LOGIN_REDIRECT_KEY, "0");
  return me;
}

/** Ends the session; resolves to where the browser must go next (the provider's logout, or the signed-out page). */
export async function logout(): Promise<string> {
  const { redirect } = (await (await request("/auth/logout", { method: "POST" })).json()) as {
    redirect: string;
  };
  return redirect;
}

export function listAudit(filters: AuditFilters, cursor: string, limit = 50): Promise<AuditPage> {
  const q = new URLSearchParams({ limit: String(limit) });
  for (const [k, v] of Object.entries(filters)) if (v) q.set(k, v);
  if (cursor) q.set("cursor", cursor);
  return getJSON<AuditPage>(`/api/v1/audit?${q}`);
}

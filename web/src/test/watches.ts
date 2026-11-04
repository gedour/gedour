import type { Credential, Watch } from "@/types/api";

export function watch(over: Partial<Watch> = {}): Watch {
  return {
    id: "w1",
    name: "Go releases",
    kind: "github_release",
    config: { repo: "golang/go", include_prereleases: false, api_url: "https://api.github.com" },
    credential_id: null,
    schedule: "15m",
    paused: false,
    owner_id: "11111111-1111-1111-1111-111111111111",
    owner_name: "Alice Liddell",
    permission: "owner",
    created_at: "2026-11-01T00:00:00Z",
    updated_at: "2026-11-01T00:00:00Z",
    ...over,
  };
}

export function credential(over: Partial<Credential> = {}): Credential {
  return {
    id: "c1",
    name: "GitHub bot",
    kind: "github_token",
    username: null,
    last4: "cdef",
    created_at: "2026-11-01T00:00:00Z",
    updated_at: "2026-11-01T00:00:00Z",
    used_by_count: 0,
    ...over,
  };
}

export const problem = (status: number, detail: string, errors: object[] = []) =>
  Response.json({ status, title: "Bad Request", detail, errors }, { status });

/** The last fetch call to path, as [url, init]. */
export function lastCall(fn: { mock: { calls: unknown[][] } }, path: string, method = "GET") {
  const call = [...fn.mock.calls].reverse().find(([input, init]) => {
    const url = new URL(String(input), "http://app.test");
    return url.pathname === path && ((init as RequestInit | undefined)?.method ?? "GET") === method;
  });
  if (!call) return undefined;
  const init = call[1] as RequestInit | undefined;
  return {
    url: new URL(String(call[0]), "http://app.test"),
    body: init?.body ? (JSON.parse(String(init.body)) as Record<string, unknown>) : undefined,
  };
}

export const pending = () => new Promise<Response>(() => {});

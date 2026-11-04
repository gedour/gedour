import { vi } from "vitest";

import type { Me } from "@/types/api";

export const ME: Me = {
  id: "11111111-1111-1111-1111-111111111111",
  email: "alice@example.test",
  display_name: "Alice Liddell",
  csrf_token: "csrf-1",
  dev_mode: false,
};

export const VERSION = {
  version: "v1.2.3",
  commit: "abc1234def",
  build_timestamp: "",
  builder: "",
  project_url: "",
};

type Route = unknown | ((url: URL, init?: RequestInit) => Response | Promise<Response>);

/** Stubs fetch by path (query ignored): a value answers JSON 200, a function answers what it returns. */
export function mockApi(routes: Record<string, Route> = {}) {
  const table: Record<string, Route> = { "/api/v1/version": VERSION, "/api/v1/me": ME, ...routes };
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input), "http://app.test");
    const route = table[url.pathname];
    if (route === undefined) return new Response("", { status: 404 });
    return typeof route === "function" ? route(url, init) : Response.json(route);
  });
  vi.stubGlobal("fetch", fn);
  return fn;
}

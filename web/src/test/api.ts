import { vi } from "vitest";

import type { Me } from "@/types/api";

export const ME: Me = {
  id: "11111111-1111-1111-1111-111111111111",
  email: "alice@example.test",
  display_name: "Alice Liddell",
  csrf_token: "csrf-1",
  dev_mode: false,
  is_admin: false,
};

export const VERSION = {
  version: "v1.2.3",
  commit: "abc1234def",
  build_timestamp: "",
  builder: "",
  project_url: "",
};

export const FILTER_REFERENCE = {
  operators: ["==", "contains", "matches"],
  builtins: ["len", "lower", "semver_gt"],
  predicates: ["any", "count"],
  limits: {
    max_length: 2000,
    max_nodes: 200,
    max_matches: 3,
    max_regex_instructions: 64,
    max_regex_repeat: 32,
    max_range: 100,
    max_array: 50,
    predicate_depth: 1,
    max_string: 4096,
    max_body: 65536,
    max_items: 200,
  },
  cost: {
    budget: 4000000,
    node: 32,
    case_mapping: 3,
    semver: 12,
    date: 2048,
    deep_value: 16,
    field_bytes: 4096,
    body_bytes: 65536,
    item_bytes: 65536,
    predicate_items: 200,
  },
  rules: ["A predicate body reads only its item: # and #.field, plus literals."],
  summary: "literals…",
};

export const CHANNEL_TYPES = [
  { type: "telegram", available: true },
  { type: "nextcloud_talk", available: true },
  { type: "email", available: true },
  { type: "webhook", available: true },
];

type Route = unknown | ((url: URL, init?: RequestInit) => Response | Promise<Response>);

/** Stubs fetch by path (query ignored): a value answers JSON 200, a function answers what it returns. */
export function mockApi(routes: Record<string, Route> = {}) {
  const table: Record<string, Route> = {
    "/api/v1/version": VERSION,
    "/api/v1/me": ME,
    "/api/v1/inbox/count": { unread: 0 },
    "/api/v1/inbox": { items: [] },
    "/api/v1/rules": { items: [] },
    "/api/v1/channels": { items: [] },
    "/api/v1/channel-types": { items: CHANNEL_TYPES },
    "/api/v1/deliveries": { items: [] },
    "/api/v1/filters/reference": FILTER_REFERENCE,
    ...routes,
  };
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input), "http://app.test");
    const route = table[url.pathname];
    if (route === undefined) return new Response("", { status: 404 });
    return typeof route === "function" ? route(url, init) : Response.json(route);
  });
  vi.stubGlobal("fetch", fn);
  return fn;
}

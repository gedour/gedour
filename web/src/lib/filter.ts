import { ApiError } from "@/api/client";

export interface FilterProblem {
  message: string;
  /** 1-based line and column, when the server located the error. */
  line?: number;
  column?: number;
}

/** The compile error out of a 400 problem at location (a filter's by default; a script's is body.source), or null. */
export function filterProblem(err: unknown, location = "body.filter"): FilterProblem | null {
  if (!(err instanceof ApiError)) return null;
  const e = err.errors.find((x) => x.location === location);
  if (!e) return null;
  const pos = e.value as { line?: unknown; column?: unknown } | null | undefined;
  const p: FilterProblem = { message: e.message ?? "invalid" };
  if (typeof pos?.line === "number" && typeof pos.column === "number") {
    p.line = pos.line;
    p.column = pos.column;
  }
  return p;
}

export interface Marker {
  line: number;
  /** The source line split around the faulty character (empty at end of line). */
  before: string;
  at: string;
  after: string;
}

/** Where to draw the error marker in src, or null without a position. */
export function marker(src: string, p: FilterProblem): Marker | null {
  if (!p.line || !p.column) return null;
  const text = src.split("\n")[p.line - 1];
  if (text === undefined) return null;
  const col = Math.min(p.column - 1, text.length);
  return {
    line: p.line,
    before: text.slice(0, col),
    at: text.slice(col, col + 1),
    after: text.slice(col + 1),
  };
}

/** Inserts text over [start, end) of src; returns the new value and the caret after the insert. */
export function insertAt(src: string, start: number, end: number, text: string) {
  return { value: src.slice(0, start) + text + src.slice(end), caret: start + text.length };
}

export interface FilterField {
  expr: string;
  hint: string;
}

/** What a filter can read (pkg/filter.Env), for the editor's reference. */
export const FILTER_FIELDS: FilterField[] = [
  { expr: "event.type", hint: 'e.g. "release.published", "tag.created", "mr.state_changed"' },
  { expr: "event.payload.tag", hint: "release or image tag" },
  { expr: "event.payload.prerelease", hint: "releases: true for a pre-release" },
  { expr: "event.payload.highest", hint: "tags: the new highest semver" },
  { expr: "event.payload.from", hint: "PR/MR changes: previous value" },
  { expr: "event.payload.to", hint: "PR/MR changes: new value" },
  { expr: "event.payload.title", hint: "PR/MR title" },
  { expr: "event.payload.body", hint: "releases: the notes, up to 64 KiB" },
  { expr: "event.payload.cves", hint: "releases: CVE ids found in the notes, uppercased" },
  { expr: "event.occurred_at", hint: "upstream time (else when gedour saw it)" },
  { expr: "watch.name", hint: "the watch's name" },
  { expr: "watch.kind", hint: '"github_release", "oci_tags"…' },
  { expr: 'semver_gt(event.payload.tag, "v1.0.0")', hint: "true when the tag is a higher version" },
  {
    expr: 'event.payload.tag matches "^v[0-9]+[.]"',
    hint: "regular expression: a small string literal, at most 3, not inside any()/all()…",
  },
  { expr: 'event.occurred_at > now() - duration("24h")', hint: "time arithmetic" },
  { expr: "len(event.payload.cves) > 0", hint: "a release that cites a CVE" },
  { expr: '"CVE-2026-1234" in event.payload.cves', hint: "a release that cites this CVE" },
  { expr: 'event.payload.body matches "(?i)breaking.change"', hint: "a pattern on the notes" },
];

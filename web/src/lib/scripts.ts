import type { CompletionContext } from "@codemirror/autocomplete";
import type { EditorState } from "@codemirror/state";

import type { FilterProblem } from "@/lib/filter";
import type { DryRunEvent, SampleEvent } from "@/types/api";

/** What a new script starts with: every capability once, nothing sent until a rule routes an event. */
export const STARTER_SOURCE = `// Runs for each event a rule sends to a script channel.
// event: type, occurred_at, payload, summary, url
// ctx: watch {id, name, kind}, rule {id, name}, delivery_id
function main(event, ctx) {
  log("received", event.type, "from", ctx.watch.name);
  const res = http.fetch("https://hooks.example.com/gedour", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ text: ctx.watch.name + ": " + event.summary, url: event.url }),
  });
  log("receiver answered", res.status);
  return { status: res.status };
}
`;

export interface ScriptCompletion {
  label: string;
  type: "variable" | "property" | "function";
  detail: string;
}

/** The globals a script sees (pkg/script), for the editor's autocomplete. */
export const SCRIPT_COMPLETIONS: ScriptCompletion[] = [
  { label: "event.type", type: "property", detail: 'e.g. "release.published"' },
  {
    label: "event.payload",
    type: "property",
    detail: "the event's data (tag, from, to, title, body, cves…)",
  },
  { label: "event.summary", type: "property", detail: 'e.g. "v1.2.0 published"' },
  { label: "event.url", type: "property", detail: "upstream page, when known" },
  { label: "event.occurred_at", type: "property", detail: "ISO time" },
  { label: "watch.id", type: "property", detail: "the watch's id" },
  { label: "watch.name", type: "property", detail: "the watch's name" },
  { label: "watch.kind", type: "property", detail: '"github_release", "oci_tags"…' },
  { label: "rule.id", type: "property", detail: "null in a dry run" },
  { label: "rule.name", type: "property", detail: "the rule that routed the event" },
  { label: "delivery_id", type: "variable", detail: "0 in a dry run" },
  { label: "log", type: "function", detail: "log(...values): a line in the delivery's logs" },
  {
    label: "http.fetch",
    type: "function",
    detail: "http.fetch(url, {method, headers, body}) → {status, ok, headers, text}",
  },
];

export interface Sample {
  label: string;
  sample: SampleEvent;
}

/** One made-up event per common type, for a dry run before any real event exists. */
export const SAMPLES: Sample[] = [
  {
    label: "Release published",
    sample: {
      type: "release.published",
      payload: {
        tag: "v2.105.0",
        name: "GitHub CLI 2.105.0",
        prerelease: false,
        url: "https://github.com/cli/cli/releases/tag/v2.105.0",
        body: "## Security\n\nFixes CVE-2099-1234.",
        cves: ["CVE-2099-1234"],
      },
      watch: { name: "GitHub CLI releases", kind: "github_release" },
    },
  },
  {
    label: "Tag created",
    sample: {
      type: "tag.created",
      payload: { tag: "18.1", highest: true },
      watch: { name: "Postgres tags", kind: "oci_tags" },
    },
  },
  {
    label: "Merge request merged",
    sample: {
      type: "mr.state_changed",
      payload: {
        iid: 12,
        title: "Add retries",
        from: "opened",
        to: "merged",
        url: "https://gitlab.com/group/project/-/merge_requests/12",
      },
      watch: { name: "Retries MR", kind: "gitlab_mr" },
    },
  },
  {
    label: "Pull request CI failing",
    sample: {
      type: "pr.ci_changed",
      payload: {
        number: 42,
        title: "Fix flaky test",
        from: "pending",
        to: "failure",
        url: "https://github.com/cli/cli/pull/42",
      },
      watch: { name: "Flaky test PR", kind: "github_pr" },
    },
  },
];

/** The picker's value for a recent event or a built-in sample. */
export type SampleKey = `event:${number}` | `sample:${number}`;

export function dryRunEventOf(key: SampleKey): DryRunEvent {
  const [kind, n] = key.split(":") as ["event" | "sample", string];
  if (kind === "event") return { event_id: Number(n) };
  const s = SAMPLES[Number(n)] ?? SAMPLES[0];
  return { sample: (s as (typeof SAMPLES)[number]).sample };
}

/** Completions for the script's globals: dotted names complete as one word. */
export function scriptCompletions(ctx: CompletionContext) {
  const word = ctx.matchBefore(/[\w.]+/);
  if (!word && !ctx.explicit) return null;
  return { from: word?.from ?? ctx.pos, options: SCRIPT_COMPLETIONS, validFor: /^[\w.]*$/ };
}

/** Where problem points in state's document, clamped inside it. */
export function problemRange(state: EditorState, p: FilterProblem): { from: number; to: number } {
  const doc = state.doc;
  const line = doc.line(Math.min(Math.max(p.line ?? 1, 1), doc.lines));
  const from = Math.min(line.from + Math.max((p.column ?? 1) - 1, 0), line.to);
  return { from, to: Math.min(from + 1, line.to) };
}

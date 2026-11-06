import { ApiError } from "@/api/client";

/**
 * Field errors of a 4xx problem keyed by form field: "body.config.repo" → "repo", "body.name" → "name".
 * Problems without a body location, and failures with no field to blame (a network drop, a bare 500), land
 * under "" (shown at the top of the form).
 */
export function fieldErrors(err: unknown): Record<string, string> {
  if (!(err instanceof ApiError)) return { "": problemText(err) };
  const out: Record<string, string> = {};
  for (const e of err.errors) {
    const loc = e.location ?? "";
    const key = loc.startsWith("body.") ? loc.slice(5).replace(/^config\./, "") : "";
    const msg = e.message ?? "invalid";
    // "body" alone is a missing required property: its message names the field.
    const missing = /required property (\w+)/.exec(msg);
    const field = loc === "body" && missing?.[1] ? missing[1] : key;
    out[field] ??= field
      ? msg.replace(/^expected required property \w+ to be present$/, "required")
      : msg;
  }
  if (Object.keys(out).length === 0) out[""] = problemText(err);
  return out;
}

/** Messages of the errors no rendered field shows (their key is not in shown), for the form's top alert. */
export function unplaced(errors: Record<string, string>, shown: string[]): string[] {
  return Object.entries(errors)
    .filter(([k]) => !shown.includes(k))
    .map(([k, msg]) => (k ? `${k}: ${msg}` : msg));
}

/** The problem's own words when the server gave some, else the generic error message. */
export function problemText(err: unknown): string {
  if (err instanceof ApiError) return err.detail || err.message;
  return err instanceof Error ? err.message : String(err);
}

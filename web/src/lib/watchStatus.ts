import { formatRelative } from "@/lib/time";
import type { Watch } from "@/types/api";

export type StatusKind = "ok" | "error" | "rate_limited" | "paused" | "pending";

export const STATUS: Record<StatusKind, { label: string; dot: string; text: string }> = {
  ok: { label: "OK", dot: "bg-emerald-500", text: "text-emerald-700 dark:text-emerald-300" },
  error: { label: "Error", dot: "bg-rose-500", text: "text-rose-700 dark:text-rose-300" },
  rate_limited: {
    label: "Rate limited",
    dot: "bg-amber-500",
    text: "text-amber-700 dark:text-amber-300",
  },
  paused: { label: "Paused", dot: "bg-slate-400", text: "text-muted" },
  pending: {
    label: "Pending first run",
    dot: "bg-transparent ring-2 ring-sky-500 ring-inset",
    text: "text-sky-700 dark:text-sky-300",
  },
};

type StatusFields = Pick<Watch, "paused" | "last_status">;
type NextRunFields = Pick<Watch, "paused" | "next_run_at"> & { running?: boolean };

/** Paused wins over the last outcome: a paused watch does not run, whatever happened last. */
export function statusOf(w: StatusFields): StatusKind {
  if (w.paused) return "paused";
  return w.last_status ?? "pending";
}

/** "running now", "paused", "due now" or "in 12 minutes". */
export function nextRunText(w: NextRunFields, now = new Date()): string {
  if (w.running) return "running now";
  if (w.paused) return "paused";
  const next = new Date(w.next_run_at);
  if (next.getTime() <= now.getTime() + 1000) return "due now";
  return formatRelative(next, now);
}

/** The list's wording: "Running now", "Due now", "Next in 12 minutes". */
export function nextRunLabel(
  w: Pick<Watch, "paused" | "next_run_at" | "running">,
  now = new Date(),
) {
  const t = nextRunText(w, now);
  return t.startsWith("in ") ? `Next ${t}` : t.charAt(0).toUpperCase() + t.slice(1);
}

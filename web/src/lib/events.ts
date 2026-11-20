import type { WatchEvent } from "@/types/api";

export type EventTone = "release" | "change" | "good" | "bad" | "neutral";

export interface EventSummary {
  /** Short type label for the badge, e.g. "Published". */
  badge: string;
  tone: EventTone;
  /** Human sentence, e.g. "v1.24.0 published", "MR !12 merged". */
  text: string;
  /** Upstream page, when the event carries one. */
  url?: string;
}

const str = (v: unknown) => (typeof v === "string" ? v : v == null ? "" : JSON.stringify(v));

const REVIEW: Record<string, string> = {
  approved: "approved",
  changes_requested: "changes requested",
  review_required: "review required",
  none: "no review",
};
const CI: Record<string, string> = {
  success: "passing",
  failure: "failing",
  pending: "running",
  none: "none",
};

const GOOD = new Set(["approved", "success", "merged"]);
const BAD = new Set(["failure", "failed", "changes_requested", "canceled", "closed"]);

function tone(to: string): EventTone {
  if (GOOD.has(to)) return "good";
  if (BAD.has(to)) return "bad";
  return "change";
}

/** opened→closed reads "closed", closed→open(ed) "reopened". */
function transition(from: string, to: string): string {
  if ((to === "open" || to === "opened") && (from === "closed" || from === "locked")) {
    return "reopened";
  }
  return to === "open" ? "opened" : to;
}

interface Approvals {
  approved?: boolean;
  count?: number;
}

// GitHub's reaction kinds and their emoji, in GitHub's order (pkg/notify/message.go reactions).
const REACTIONS: [string, string][] = [
  ["+1", "👍"],
  ["-1", "👎"],
  ["laugh", "😄"],
  ["hooray", "🎉"],
  ["confused", "😕"],
  ["heart", "❤️"],
  ["rocket", "🚀"],
  ["eyes", "👀"],
];
const REACTED_ON: Record<string, string> = {
  "issue.reacted": "issue #",
  "pr.reacted": "PR #",
  "discussion.reacted": "discussion #",
};

function plural(n: number, word: string) {
  return `${n} ${word}${n === 1 ? "" : "s"}`;
}

/** The collector stops counting missed runs at 1000 (pkg/collect/schedules.go maxMissed). */
export function missedCount(n: number) {
  return n >= 1000 ? "1000+" : String(n);
}

/** GitHub's disabled_inactivity reads "inactivity" (pkg/notify/message.go disabledReason). */
const disabledReason = (state: string) => state.replace(/^disabled_/, "").replaceAll("_", " ");

/** An RFC 3339 payload time in UTC to the minute, anything else as is (pkg/notify/message.go when). */
function when(v: unknown): string {
  const s = str(v);
  const t = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$/.test(s)
    ? new Date(s)
    : null;
  return t && !Number.isNaN(t.getTime())
    ? `${t.toISOString().slice(0, 16).replace("T", " ")} UTC`
    : s;
}

/** Go's path.Base: the last element, "." for an empty path. */
function baseName(p: string): string {
  const t = p.replace(/\/+$/, "");
  if (t === "") return p ? "/" : ".";
  return t.slice(t.lastIndexOf("/") + 1);
}

export function summarizeEvent(e: WatchEvent): EventSummary {
  const p = e.payload;
  const url = typeof p.url === "string" && /^https?:\/\//.test(p.url) ? p.url : undefined;
  const from = str(p.from);
  const to = str(p.to);
  const pr = `PR #${str(p.number)}`;
  const mr = `MR !${str(p.iid)}`;
  const issue = `Issue #${str(p.number)}`;
  const discussion = `Discussion #${str(p.number)}`;
  const item = e.type.startsWith("discussion.") ? discussion : issue;
  const workflow = baseName(str(p.path));
  switch (e.type) {
    case "release.published":
      return {
        badge: "Published",
        tone: "release",
        text: `${str(p.tag)} published${p.prerelease ? " (pre-release)" : ""}`,
        url,
      };
    case "release.promoted":
      return { badge: "Promoted", tone: "release", text: `${str(p.tag)} promoted to release`, url };
    case "pr.state_changed":
    case "mr.state_changed": {
      const t = transition(from, to);
      return {
        badge: "State",
        tone: tone(t),
        text: `${e.type.startsWith("pr") ? pr : mr} ${t}`,
        url,
      };
    }
    case "pr.draft_changed":
    case "mr.draft_changed":
      return {
        badge: "Draft",
        tone: "change",
        text: `${e.type.startsWith("pr") ? pr : mr} ${p.to ? "converted to draft" : "marked ready for review"}`,
        url,
      };
    case "pr.review_changed":
      return {
        badge: "Review",
        tone: tone(to),
        text: `${pr} review: ${REVIEW[to] ?? to}`,
        url,
      };
    case "pr.ci_changed":
      return { badge: "CI", tone: tone(to), text: `${pr} CI ${CI[to] ?? to}`, url };
    case "pr.commented":
      return { badge: "Comment", tone: "change", text: `${pr} commented by ${str(p.author)}`, url };
    case "issue.opened":
      return {
        badge: "New issue",
        tone: "change",
        text: `${issue} opened by ${str(p.author)}`,
        url,
      };
    case "issue.state_changed":
    case "discussion.state_changed": {
      const t = transition(from, to);
      const reason = str(p.state_reason).replaceAll("_", " ");
      return {
        badge: "State",
        // Closed as completed (resolved for a discussion) is the happy ending; other reasons keep the closed tone.
        tone:
          t === "closed" && (p.state_reason === "completed" || p.state_reason === "resolved")
            ? "good"
            : tone(t),
        text: `${item} ${t}${t === "closed" && reason ? ` (${reason})` : ""}`,
        url,
      };
    }
    case "issue.labels_changed":
    case "discussion.labels_changed": {
      const labels = Array.isArray(p.to) ? p.to.map(str) : [];
      return {
        badge: "Labels",
        tone: "change",
        text: `${item} labels: ${labels.length ? labels.join(", ") : "none"}`,
        url,
      };
    }
    case "issue.commented":
      return {
        badge: "Comment",
        tone: "change",
        text: `${issue} commented by ${str(p.author)}`,
        url,
      };
    case "pr.opened":
      return { badge: "New PR", tone: "change", text: `${pr} opened by ${str(p.author)}`, url };
    case "discussion.opened":
      return {
        badge: "New discussion",
        tone: "change",
        text: `${discussion} opened by ${str(p.author)}`,
        url,
      };
    case "discussion.answer_changed":
      return {
        badge: "Answer",
        tone: to ? "good" : "change",
        text: `Answer ${to ? "chosen" : "removed"} on discussion #${str(p.number)}`,
        url,
      };
    case "discussion.commented":
      return {
        badge: "Comment",
        tone: "change",
        text: `${discussion} ${p.reply_to ? "reply" : "commented"} by ${str(p.author)}`,
        url,
      };
    case "issue.reacted":
    case "pr.reacted":
    case "discussion.reacted": {
      const added = (p.added ?? {}) as Record<string, number>;
      const rises = REACTIONS.filter(([k]) => (added[k] ?? 0) > 0).map(
        ([k, emoji]) => `+${added[k]} ${emoji}`,
      );
      const on = `${REACTED_ON[e.type]}${str(p.number)}`;
      return {
        badge: "Reactions",
        tone: "change",
        text: `${rises.join(" ")} on ${p.target === "comment" ? `a comment of ${on}` : on}`,
        url,
      };
    }
    case "mr.approval_changed": {
      const a = (p.to ?? {}) as Approvals;
      const count = plural(a.count ?? 0, "approval");
      return {
        badge: "Approval",
        tone: a.approved ? "good" : "change",
        text: a.approved ? `${mr} approved (${count})` : `${mr} not approved (${count})`,
        url,
      };
    }
    case "mr.pipeline_changed":
      return { badge: "Pipeline", tone: tone(to), text: `${mr} pipeline ${to}`, url };
    case "tag.created":
      return {
        badge: "New tag",
        tone: "release",
        text: `tag ${str(p.tag)} created${p.highest ? " (new highest)" : ""}`,
      };
    case "tag.removed":
      return { badge: "Removed", tone: "neutral", text: `tag ${str(p.tag)} removed` };
    case "schedule.disabled":
      return {
        badge: "Disabled",
        tone: "bad",
        text: `Scheduled workflow ${workflow} disabled (${disabledReason(str(p.state))})`,
        url,
      };
    case "schedule.enabled":
      return {
        badge: "Enabled",
        tone: "good",
        text: `Scheduled workflow ${workflow} enabled`,
        url,
      };
    case "schedule.stale": {
      const n = typeof p.missed === "number" ? Math.trunc(p.missed) : 0;
      const missed = n >= 1000 ? `${missedCount(n)} runs` : plural(n, "run");
      // The oldest missed time: the count may start after last_alive_at, at the workflow's last edit.
      const since = when(p.expected_at);
      return {
        badge: "Stale",
        tone: "bad",
        text: `${workflow} missed ${missed}${since ? ` since ${since}` : ""}`,
        url,
      };
    }
    case "schedule.recovered":
      return {
        badge: "Recovered",
        tone: "good",
        text: `Scheduled workflow ${workflow} runs again`,
        url,
      };
    case "schedule.reenabled":
      return {
        badge: "Re-enabled",
        tone: "good",
        text: `Scheduled workflow ${workflow} re-enabled after ${disabledReason(from)}`,
        url,
      };
    case "schedule.disable_soon":
      return {
        badge: "Turning off",
        tone: "bad",
        text: `Scheduled workflows of ${str(p.repo)} turn off on ${when(p.disable_at)} without a commit`,
        url,
      };
    default:
      return { badge: e.type, tone: "neutral", text: e.type, url };
  }
}

export const TONE_CLASS: Record<EventTone, string> = {
  release: "bg-beacon-soft text-ink ring-beacon/30",
  change: "bg-sky-500/10 text-sky-800 ring-sky-600/25 dark:text-sky-300",
  good: "bg-emerald-500/10 text-emerald-800 ring-emerald-600/25 dark:text-emerald-300",
  bad: "bg-rose-500/10 text-rose-800 ring-rose-600/25 dark:text-rose-300",
  neutral: "bg-line/60 text-muted ring-line",
};

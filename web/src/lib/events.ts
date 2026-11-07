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

function plural(n: number, word: string) {
  return `${n} ${word}${n === 1 ? "" : "s"}`;
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

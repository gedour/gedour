import { describe, expect, it } from "vitest";

import { summarizeEvent } from "@/lib/events";
import type { WatchEvent } from "@/types/api";

const ev = (type: string, payload: Record<string, unknown>): WatchEvent => ({
  id: 1,
  type,
  occurred_at: null,
  observed_at: "2026-11-04T00:00:00Z",
  payload,
});

const pr = { number: 12, title: "t", url: "https://github.com/a/b/pull/12" };
const mr = { iid: 7, title: "t", url: "https://gitlab.com/a/b/-/merge_requests/7" };

describe("summarizeEvent", () => {
  it.each([
    [
      ev("release.published", { tag: "v1.24.0", url: "https://x/r" }),
      "Published",
      "v1.24.0 published",
      "release",
    ],
    [
      ev("release.published", { tag: "v2.0.0-rc.1", prerelease: true }),
      "Published",
      "v2.0.0-rc.1 published (pre-release)",
      "release",
    ],
    [
      ev("release.promoted", { tag: "v2.0.0" }),
      "Promoted",
      "v2.0.0 promoted to release",
      "release",
    ],
    [
      ev("pr.state_changed", { ...pr, from: "open", to: "merged" }),
      "State",
      "PR #12 merged",
      "good",
    ],
    [
      ev("pr.state_changed", { ...pr, from: "open", to: "closed" }),
      "State",
      "PR #12 closed",
      "bad",
    ],
    [
      ev("pr.state_changed", { ...pr, from: "closed", to: "open" }),
      "State",
      "PR #12 reopened",
      "change",
    ],
    [ev("pr.state_changed", { ...pr, from: "", to: "open" }), "State", "PR #12 opened", "change"],
    [
      ev("pr.draft_changed", { ...pr, from: true, to: false }),
      "Draft",
      "PR #12 marked ready for review",
      "change",
    ],
    [
      ev("pr.draft_changed", { ...pr, from: false, to: true }),
      "Draft",
      "PR #12 converted to draft",
      "change",
    ],
    [
      ev("pr.review_changed", { ...pr, from: "none", to: "changes_requested" }),
      "Review",
      "PR #12 review: changes requested",
      "bad",
    ],
    [
      ev("pr.review_changed", { ...pr, from: "none", to: "odd" }),
      "Review",
      "PR #12 review: odd",
      "change",
    ],
    [
      ev("pr.ci_changed", { ...pr, from: "pending", to: "success" }),
      "CI",
      "PR #12 CI passing",
      "good",
    ],
    [
      ev("pr.ci_changed", { ...pr, from: "pending", to: "weird" }),
      "CI",
      "PR #12 CI weird",
      "change",
    ],
    [
      ev("pr.commented", { ...pr, author: "bob", excerpt: "Looks good." }),
      "Comment",
      "PR #12 commented by bob",
      "change",
    ],
    // 008-AC12: the two new-item events, mirrored in pkg/notify/message.go.
    [
      ev("issue.opened", { ...pr, author: "alice", labels: ["bug"] }),
      "New issue",
      "Issue #12 opened by alice",
      "change",
    ],
    // 008-AC12: the github_issue events, mirrored in pkg/notify/message.go.
    [
      ev("issue.state_changed", { ...pr, from: "open", to: "closed", state_reason: "not_planned" }),
      "State",
      "Issue #12 closed (not planned)",
      "bad",
    ],
    [
      ev("issue.state_changed", { ...pr, from: "open", to: "closed", state_reason: "completed" }),
      "State",
      "Issue #12 closed (completed)",
      "good",
    ],
    [
      ev("issue.state_changed", { ...pr, from: "open", to: "closed", state_reason: "" }),
      "State",
      "Issue #12 closed",
      "bad",
    ],
    [
      ev("issue.state_changed", { ...pr, from: "closed", to: "closed", state_reason: "duplicate" }),
      "State",
      "Issue #12 closed (duplicate)",
      "bad",
    ],
    [
      ev("issue.state_changed", { ...pr, from: "closed", to: "closed", state_reason: "completed" }),
      "State",
      "Issue #12 closed (completed)",
      "good",
    ],
    // A server keeping the old reason on reopen: still a reopen, not a happy close.
    [
      ev("issue.state_changed", { ...pr, from: "closed", to: "open", state_reason: "completed" }),
      "State",
      "Issue #12 reopened",
      "change",
    ],
    [
      ev("issue.state_changed", { ...pr, from: "closed", to: "open", state_reason: "reopened" }),
      "State",
      "Issue #12 reopened",
      "change",
    ],
    [
      ev("issue.labels_changed", { ...pr, from: ["bug"], to: ["bug", "triage"] }),
      "Labels",
      "Issue #12 labels: bug, triage",
      "change",
    ],
    [
      ev("issue.labels_changed", { ...pr, from: ["bug"], to: [] }),
      "Labels",
      "Issue #12 labels: none",
      "change",
    ],
    [
      ev("issue.commented", { ...pr, author: "alice", excerpt: "Same here." }),
      "Comment",
      "Issue #12 commented by alice",
      "change",
    ],
    [
      ev("pr.opened", { ...pr, author: "alice", labels: [] }),
      "New PR",
      "PR #12 opened by alice",
      "change",
    ],
    [
      ev("mr.state_changed", { ...mr, from: "opened", to: "merged" }),
      "State",
      "MR !7 merged",
      "good",
    ],
    [
      ev("mr.state_changed", { ...mr, from: "locked", to: "opened" }),
      "State",
      "MR !7 reopened",
      "change",
    ],
    [
      ev("mr.draft_changed", { ...mr, from: false, to: true }),
      "Draft",
      "MR !7 converted to draft",
      "change",
    ],
    [
      ev("mr.approval_changed", { ...mr, to: { approved: true, count: 2 } }),
      "Approval",
      "MR !7 approved (2 approvals)",
      "good",
    ],
    [
      ev("mr.approval_changed", { ...mr, to: { approved: false, count: 1 } }),
      "Approval",
      "MR !7 not approved (1 approval)",
      "change",
    ],
    [
      ev("mr.approval_changed", { ...mr }),
      "Approval",
      "MR !7 not approved (0 approvals)",
      "change",
    ],
    [
      ev("mr.pipeline_changed", { ...mr, from: "running", to: "failed" }),
      "Pipeline",
      "MR !7 pipeline failed",
      "bad",
    ],
    [
      ev("tag.created", { tag: "18.1", highest: true }),
      "New tag",
      "tag 18.1 created (new highest)",
      "release",
    ],
    [
      ev("tag.created", { tag: "18.1-alpine", highest: false }),
      "New tag",
      "tag 18.1-alpine created",
      "release",
    ],
    [ev("tag.removed", { tag: "10" }), "Removed", "tag 10 removed", "neutral"],
    [ev("future.thing", { x: 1 }), "future.thing", "future.thing", "neutral"],
  ])("%#: %s", (e, badge, text, tone) => {
    const s = summarizeEvent(e);
    expect([s.badge, s.text, s.tone]).toEqual([badge, text, tone]);
  });

  it("links only http(s) upstream URLs", () => {
    expect(summarizeEvent(ev("pr.ci_changed", { ...pr, to: "success" })).url).toBe(pr.url);
    expect(
      summarizeEvent(ev("release.published", { tag: "v1", url: "javascript:alert(1)" })).url,
    ).toBeUndefined();
    expect(summarizeEvent(ev("tag.created", { tag: "1" })).url).toBeUndefined();
  });
});

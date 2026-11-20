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
const wf = {
  workflow: "Mirror",
  path: ".github/workflows/nightly.yml",
  url: "https://github.com/o/r/blob/main/.github/workflows/nightly.yml",
};
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
    // 011-AC16: discussion.opened, mirrored in pkg/notify/message.go.
    [
      ev("discussion.opened", { ...pr, author: "alice", category: "Ideas", labels: [] }),
      "New discussion",
      "Discussion #12 opened by alice",
      "change",
    ],
    // 011-AC16: the github_discussion events, mirrored in pkg/notify/message.go.
    [
      ev("discussion.state_changed", {
        ...pr,
        from: "open",
        to: "closed",
        state_reason: "resolved",
      }),
      "State",
      "Discussion #12 closed (resolved)",
      "good",
    ],
    [
      ev("discussion.state_changed", {
        ...pr,
        from: "closed",
        to: "closed",
        state_reason: "outdated",
      }),
      "State",
      "Discussion #12 closed (outdated)",
      "bad",
    ],
    [
      ev("discussion.state_changed", {
        ...pr,
        from: "closed",
        to: "open",
        state_reason: "reopened",
      }),
      "State",
      "Discussion #12 reopened",
      "change",
    ],
    [
      ev("discussion.answer_changed", { ...pr, from: "", to: "https://x/1", chosen_by: "bob" }),
      "Answer",
      "Answer chosen on discussion #12",
      "good",
    ],
    [
      ev("discussion.answer_changed", { ...pr, from: "https://x/1", to: "", chosen_by: "" }),
      "Answer",
      "Answer removed on discussion #12",
      "change",
    ],
    [
      ev("discussion.labels_changed", { ...pr, from: [], to: ["question"] }),
      "Labels",
      "Discussion #12 labels: question",
      "change",
    ],
    [
      ev("discussion.commented", { ...pr, author: "alice", excerpt: "Try this." }),
      "Comment",
      "Discussion #12 commented by alice",
      "change",
    ],
    [
      ev("discussion.commented", {
        ...pr,
        author: "bob",
        excerpt: "Thanks!",
        reply_to: "https://x/1",
      }),
      "Comment",
      "Discussion #12 reply by bob",
      "change",
    ],
    // 011-AC16: the reacted events, rises in GitHub's order, mirrored in pkg/notify/message.go.
    [
      ev("issue.reacted", { ...pr, target: "item", added: { heart: 2 } }),
      "Reactions",
      "+2 ❤️ on issue #12",
      "change",
    ],
    [
      ev("pr.reacted", { ...pr, number: 7, target: "comment", added: { "+1": 1 } }),
      "Reactions",
      "+1 👍 on a comment of PR #7",
      "change",
    ],
    [
      ev("discussion.reacted", {
        ...pr,
        number: 3,
        target: "item",
        added: { rocket: 1, heart: 2, eyes: 0 },
      }),
      "Reactions",
      "+2 ❤️ +1 🚀 on discussion #3",
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
    // 019-AC11: the github_schedules events, mirrored in pkg/notify/message.go.
    [
      ev("schedule.disabled", { ...wf, state: "disabled_inactivity", last_alive_at: null }),
      "Disabled",
      "Scheduled workflow nightly.yml disabled (inactivity)",
      "bad",
    ],
    [
      ev("schedule.disabled", { ...wf, state: "disabled_manually" }),
      "Disabled",
      "Scheduled workflow nightly.yml disabled (manually)",
      "bad",
    ],
    [
      ev("schedule.enabled", { ...wf, state: "active" }),
      "Enabled",
      "Scheduled workflow nightly.yml enabled",
      "good",
    ],
    [
      ev("schedule.stale", {
        ...wf,
        missed: 3,
        last_alive_at: "2026-11-04T08:05:00Z",
        expected_at: "2026-11-04T09:00:00Z",
      }),
      "Stale",
      "nightly.yml missed 3 runs since 2026-11-04 09:00 UTC",
      "bad",
    ],
    // Edited at 12:00 after its 08:05 success: the count starts at 13:00, and so does "since".
    [
      ev("schedule.stale", {
        ...wf,
        missed: 3,
        last_alive_at: "2026-11-04T08:05:00Z",
        expected_at: "2026-11-04T13:00:00Z",
      }),
      "Stale",
      "nightly.yml missed 3 runs since 2026-11-04 13:00 UTC",
      "bad",
    ],
    [
      ev("schedule.stale", { ...wf, missed: 3, last_alive_at: "2026-11-04T08:05:00Z" }),
      "Stale",
      "nightly.yml missed 3 runs",
      "bad",
    ],
    [
      ev("schedule.stale", { ...wf, missed: 1, last_alive_at: null }),
      "Stale",
      "nightly.yml missed 1 run",
      "bad",
    ],
    // Go's path.Base and a missing count read the same as in pkg/notify/message.go.
    [
      ev("schedule.stale", {
        ...wf,
        missed: 336,
        last_alive_at: "2026-10-21T08:05:00Z",
        expected_at: "2026-10-21T09:00:00Z",
      }),
      "Stale",
      "nightly.yml missed 336 runs since 2026-10-21 09:00 UTC",
      "bad",
    ],
    [
      ev("schedule.stale", { ...wf, missed: 1000, last_alive_at: null }),
      "Stale",
      "nightly.yml missed 1000+ runs",
      "bad",
    ],
    [
      ev("schedule.stale", {
        ...wf,
        missed: 1000,
        last_alive_at: null,
        expected_at: "2026-01-01T00:01:00Z",
      }),
      "Stale",
      "nightly.yml missed 1000+ runs since 2026-01-01 00:01 UTC",
      "bad",
    ],
    [ev("schedule.stale", { path: "" }), "Stale", ". missed 0 runs", "bad"],
    [ev("schedule.enabled", { path: "/" }), "Enabled", "Scheduled workflow / enabled", "good"],
    [
      ev("schedule.recovered", wf),
      "Recovered",
      "Scheduled workflow nightly.yml runs again",
      "good",
    ],
    [
      ev("schedule.reenabled", { ...wf, from: "disabled_inactivity" }),
      "Re-enabled",
      "Scheduled workflow nightly.yml re-enabled after inactivity",
      "good",
    ],
    [
      ev("schedule.disable_soon", {
        repo: "o/r",
        last_activity_at: "2026-09-25T10:00:00Z",
        disable_at: "2026-11-24T10:00:00+01:00",
        workflows: [".github/workflows/nightly.yml"],
      }),
      "Turning off",
      "Scheduled workflows of o/r turn off on 2026-11-24 09:00 UTC without a commit",
      "bad",
    ],
    [
      ev("schedule.disable_soon", { repo: "o/r", disable_at: "soon" }),
      "Turning off",
      "Scheduled workflows of o/r turn off on soon without a commit",
      "bad",
    ],
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

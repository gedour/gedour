import { describe, expect, it } from "vitest";

import { stateLine } from "@/lib/watchState";

describe("stateLine", () => {
  it.each([
    [
      "github_repo",
      {
        releases: [
          { tag: "v2", draft: true },
          { tag: "v1", draft: false },
        ],
      },
      "2 releases, latest v1",
    ],
    ["gitlab_release", { releases: null }, "0 releases"],
    ["gitlab_release", { releases: [{ tag: "v1", draft: false }] }, "1 release, latest v1"],
    [
      "github_pr",
      { state: "open", draft: true, review_decision: "review_required", ci: "pending" },
      "open (draft) · review review required · CI pending",
    ],
    ["github_issue", { state: "open", state_reason: "", labels: ["bug"] }, "open · bug"],
    [
      "github_issue",
      { state: "closed", state_reason: "not_planned", labels: ["bug", "wontfix"] },
      "closed (not planned) · bug, wontfix",
    ],
    ["github_issue", { state: "open", state_reason: "reopened", labels: null }, "open"],
    [
      "github_discussion",
      { state: "closed", state_reason: "resolved", labels: ["question"], category: "Q&A" },
      "closed (resolved) · question",
    ],
    [
      "gitlab_mr",
      {
        state: "merged",
        draft: false,
        approvals: { approved: true, count: 1 },
        pipeline: "success",
      },
      "merged · approved · pipeline success",
    ],
    [
      "gitlab_mr",
      { state: "opened", draft: true, approvals: { approved: false, count: 0 }, pipeline: "none" },
      "opened (draft) · not approved · pipeline none",
    ],
    ["oci_tags", { tags: ["1"], count: 1, highest: "1" }, "1 tag, highest 1"],
    ["oci_tags", { tags: [], count: 0, highest: "" }, "0 tags"],
    ["github_pr", null, "empty"],
  ] as const)("%s %o", (kind, state, want) => {
    expect(stateLine(kind, state)).toBe(want);
  });
});

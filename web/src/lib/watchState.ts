import type { WatchKind } from "@/types/api";

// Mirrors of the collectors' normalized states (pkg/collect): the API types them as unknown.
export interface Release {
  tag: string;
  name: string;
  prerelease: boolean;
  draft: boolean;
  published_at: string | null;
  url: string;
}
export interface ReleaseState {
  releases: Release[] | null;
}
export interface PRState {
  number: number;
  state: string;
  draft: boolean;
  title: string;
  review_decision: string;
  ci: string;
  head_sha: string;
  url: string;
}
export interface MRState {
  iid: number;
  state: string;
  draft: boolean;
  title: string;
  approvals: { approved: boolean; count: number };
  pipeline: string;
  sha: string;
  url: string;
}
export interface TagState {
  tags: string[] | null;
  count: number;
  highest: string;
}

/** One line describing a stored state, for the history list. */
export function stateLine(kind: WatchKind, state: unknown): string {
  if (!state || typeof state !== "object") return "empty";
  switch (kind) {
    case "github_repo":
    case "gitlab_release": {
      const rs = (state as ReleaseState).releases ?? [];
      const latest = rs.find((r) => !r.draft);
      return `${rs.length} release${rs.length === 1 ? "" : "s"}${latest ? `, latest ${latest.tag}` : ""}`;
    }
    case "github_pr": {
      const s = state as PRState;
      return `${s.state}${s.draft ? " (draft)" : ""} · review ${s.review_decision.replaceAll("_", " ")} · CI ${s.ci}`;
    }
    case "gitlab_mr": {
      const s = state as MRState;
      return `${s.state}${s.draft ? " (draft)" : ""} · ${s.approvals.approved ? "approved" : "not approved"} · pipeline ${s.pipeline}`;
    }
    case "oci_tags": {
      const s = state as TagState;
      return `${s.count} tag${s.count === 1 ? "" : "s"}${s.highest ? `, highest ${s.highest}` : ""}`;
    }
  }
}

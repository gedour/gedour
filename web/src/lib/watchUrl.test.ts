import { describe, expect, it } from "vitest";

import { recognize } from "@/lib/watchUrl";

describe("recognize", () => {
  // 001-AC4: a pasted URL or reference pre-fills the kind and config of the new-watch form.
  it.each([
    ["https://github.com/golang/go", "github_repo", { repo: "golang/go" }],
    ["https://github.com/golang/go/releases", "github_repo", { repo: "golang/go" }],
    ["https://github.com/golang/go/releases/tag/go1.22.0", "github_repo", { repo: "golang/go" }],
    ["github.com/golang/go.git", "github_repo", { repo: "golang/go" }],
    ["https://github.com/golang/go/pull/123", "github_pr", { repo: "golang/go", number: 123 }],
    [
      "https://www.github.com/golang/go/pull/7/files?w=1#diff",
      "github_pr",
      { repo: "golang/go", number: 7 },
    ],
    // 008-AC13: an issue URL picks github_issue; a non-numeric or zero number falls back to the repo.
    ["https://github.com/o/r/issues/12", "github_issue", { repo: "o/r", number: 12 }],
    [
      "github.com/cli/cli/issues/9400#issuecomment-1",
      "github_issue",
      { repo: "cli/cli", number: 9400 },
    ],
    ["https://github.com/o/r/issues", "github_repo", { repo: "o/r" }],
    ["https://github.com/o/r/issues/0", "github_repo", { repo: "o/r" }],
    ["https://github.com/o/r/issues/new", "github_repo", { repo: "o/r" }],
    // 011-AC10: a discussion URL picks github_discussion; the list or a category falls back to the repo.
    ["https://github.com/o/r/discussions/12", "github_discussion", { repo: "o/r", number: 12 }],
    [
      "github.com/cli/cli/discussions/9507#discussioncomment-4100001",
      "github_discussion",
      { repo: "cli/cli", number: 9507 },
    ],
    ["https://github.com/o/r/discussions", "github_repo", { repo: "o/r" }],
    ["https://github.com/o/r/discussions/categories", "github_repo", { repo: "o/r" }],
    [
      "https://github.com/orgs/acme/packages/container/package/Api",
      "oci_tags",
      { registry: "ghcr.io", repository: "acme/api" },
    ],
    ["https://gitlab.com/gitlab-org/gitlab", "gitlab_release", { project: "gitlab-org/gitlab" }],
    [
      "https://gitlab.com/gitlab-org/gitlab/-/releases",
      "gitlab_release",
      { project: "gitlab-org/gitlab" },
    ],
    [
      "gitlab.com/grp/sub/proj/-/merge_requests/123/diffs",
      "gitlab_mr",
      { project: "grp/sub/proj", iid: 123 },
    ],
    [
      "https://git.example.com/team/app/-/merge_requests/7",
      "gitlab_mr",
      { project: "team/app", iid: 7, base_url: "https://git.example.com" },
    ],
    [
      "https://gitlab.example.org/team/sub/app",
      "gitlab_release",
      { project: "team/sub/app", base_url: "https://gitlab.example.org" },
    ],
    [
      "https://git.example.com/team/app/-/releases",
      "gitlab_release",
      { project: "team/app", base_url: "https://git.example.com" },
    ],
    [
      "https://hub.docker.com/_/postgres",
      "oci_tags",
      { registry: "docker.io", repository: "library/postgres" },
    ],
    [
      "https://hub.docker.com/r/bitnami/redis/tags",
      "oci_tags",
      { registry: "docker.io", repository: "bitnami/redis" },
    ],
    ["ghcr.io/org/img", "oci_tags", { registry: "ghcr.io", repository: "org/img" }],
    ["ghcr.io/org/img:1.2.3", "oci_tags", { registry: "ghcr.io", repository: "org/img" }],
    ["https://ghcr.io/org/img", "oci_tags", { registry: "ghcr.io", repository: "org/img" }],
    [
      "quay.io/prometheus/node-exporter@sha256:abc123",
      "oci_tags",
      { registry: "quay.io", repository: "prometheus/node-exporter" },
    ],
    [
      "https://quay.io/repository/prometheus/node-exporter?tab=tags",
      "oci_tags",
      { registry: "quay.io", repository: "prometheus/node-exporter" },
    ],
    [
      "registry.example.com:5000/team/app:latest",
      "oci_tags",
      { registry: "registry.example.com:5000", repository: "team/app" },
    ],
    [
      "docker.io/library/postgres:16",
      "oci_tags",
      { registry: "docker.io", repository: "library/postgres" },
    ],
    ["postgres:16-alpine", "oci_tags", { registry: "docker.io", repository: "library/postgres" }],
    ["bitnami/redis:7", "oci_tags", { registry: "docker.io", repository: "bitnami/redis" }],
    ["localhost:5000/app", "oci_tags", { registry: "localhost:5000", repository: "app" }],
  ])("%s", (input, kind, config) => {
    expect(recognize(input)).toEqual({ kind, config });
  });

  it.each([
    "",
    "   ",
    "not a url",
    "postgres",
    "golang/go",
    "https://github.com/golang",
    "https://gitlab.com/solo",
    "https://example.com/some/page",
    "ftp://github.com/golang/go",
    "https://hub.docker.com/search?q=x",
    "https://[bad",
    "ghcr.io/Bad Name",
    "ghcr.io/-bad",
    "https://github.com/a/%E0%A4%A",
  ])("rejects %j", (input) => {
    expect(recognize(input)).toBeNull();
  });
});

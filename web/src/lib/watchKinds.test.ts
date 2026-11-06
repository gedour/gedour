import { describe, expect, it } from "vitest";

import {
  credentialHost,
  credentialLabel,
  describeSchedule,
  fromConfig,
  kindInfo,
  targetSummary,
  toConfig,
} from "@/lib/watchKinds";

describe("toConfig / fromConfig", () => {
  it("sends only the kind's fields, trimmed, numbers as numbers, blanks left out", () => {
    const values = { repo: " golang/go ", number: "12", api_url: "", project: "x/y", extra: "z" };
    expect(toConfig("github_pr", values)).toEqual({ repo: "golang/go", number: 12 });
    expect(toConfig("github_repo", values)).toEqual({
      repo: "golang/go",
      include_prereleases: false,
    });
    expect(toConfig("oci_tags", { repository: "postgres", semver_only: true })).toEqual({
      repository: "postgres",
      semver_only: true,
    });
  });

  it("reads a stored config back into form values", () => {
    expect(fromConfig({ repo: "a/b", number: 3, include_prereleases: true, x: null })).toEqual({
      repo: "a/b",
      number: "3",
      include_prereleases: true,
      x: "",
    });
  });
});

describe("targetSummary", () => {
  it.each([
    ["github_repo", { repo: "golang/go" }, "golang/go releases"],
    ["github_pr", { repo: "golang/go", number: 5 }, "golang/go #5"],
    ["gitlab_release", { project: "a/b" }, "a/b releases"],
    ["gitlab_mr", { project: "a/b", iid: 9 }, "a/b !9"],
    ["oci_tags", { registry: "docker.io", repository: "library/postgres" }, "postgres tags"],
    ["oci_tags", { registry: "docker.io", repository: "bitnami/redis" }, "bitnami/redis tags"],
    ["oci_tags", { repository: "library/redis" }, "redis tags"],
    ["oci_tags", { registry: "ghcr.io", repository: "org/img" }, "ghcr.io/org/img tags"],
  ] as const)("%s %j", (kind, config, want) => {
    expect(targetSummary({ kind, config })).toBe(want);
  });

  it("labels kinds and credentials", () => {
    expect(kindInfo("gitlab_mr").credential).toBe("gitlab_token");
    expect(kindInfo("nope" as "oci_tags").kind).toBe("github_repo");
    expect(credentialLabel("registry_basic")).toBe("Registry login");
    expect(credentialLabel("x" as "github_token")).toBe("x");
  });
});

describe("describeSchedule", () => {
  it.each([
    ["15m", "Every 15 minutes"],
    ["1h", "Every 1 hour"],
    ["1h30m", "Every 1 hour 30 minutes"],
    [" 0 9 * * * ", "Every day at 09:00"],
    ["30 18 * * 1-5", "At 18:30 on weekdays"],
    ["0 10 * * 0,6", "At 10:00 on weekends"],
    ["0 8 * * 1,3", "At 08:00 on Monday, Wednesday"],
    ["0 8 * * 7", "At 08:00 on Sunday"],
    ["*/10 * * * *", "Every 10 minutes"],
    ["5 * * * *", "Every hour at minute 5"],
    ["0 6 1 * *", "At 06:00 on day 1 of every month"],
  ])("%j", (raw, want) => {
    expect(describeSchedule(raw)).toBe(want);
  });

  it.each([
    "",
    "soon",
    "0 9 * * mon-fri",
    "0 9 * 1 *",
    "0 9 1 * 1",
    "@daily",
    "*/5 9 * * *",
    "61 * * * *",
    "0 24 * * *",
    "0 9 32 * *",
  ])("cannot phrase %j", (raw) => {
    expect(describeSchedule(raw)).toBeNull();
  });
});

describe("credentialHost", () => {
  it.each([
    ["github_repo", {}, "api.github.com"],
    ["github_pr", { api_url: " https://ghe.example.com:8443/api/v3 " }, "ghe.example.com:8443"],
    ["gitlab_mr", {}, "gitlab.com"],
    ["gitlab_release", { base_url: "https://git.example.com" }, "git.example.com"],
    ["gitlab_release", { base_url: "not a url" }, "not a url"],
    ["oci_tags", {}, "docker.io"],
    ["oci_tags", { registry: "GHCR.io" }, "ghcr.io"],
    ["oci_tags", { registry: "index.docker.io" }, "docker.io"],
    ["oci_tags", { registry: "Registry-1.Docker.io" }, "docker.io"],
    ["oci_tags", { registry: "registry.hub.docker.com" }, "docker.io"],
  ] as const)("%s %j", (kind, values, want) => {
    expect(credentialHost(kind, values)).toBe(want);
  });
});

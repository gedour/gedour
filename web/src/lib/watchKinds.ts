import {
  CircleDot,
  Container,
  GitMerge,
  GitPullRequest,
  MessagesSquare,
  Rocket,
  Tag,
  type LucideIcon,
} from "lucide-react";

import type { CredentialKind, Watch, WatchConfig, WatchKind } from "@/types/api";

export interface KindInfo {
  kind: WatchKind;
  label: string;
  blurb: string;
  icon: LucideIcon;
  credential: CredentialKind;
  /** Mirrors the server's DefaultScheduleFor: unauthenticated GitHub allows 60 requests an hour. */
  defaultSchedule: string;
}

export const KINDS: KindInfo[] = [
  {
    kind: "github_repo",
    label: "GitHub repository",
    blurb: "Releases, new issues, PRs and discussions of a repository",
    icon: Rocket,
    credential: "github_token",
    defaultSchedule: "1h",
  },
  {
    kind: "github_pr",
    label: "GitHub pull request",
    blurb: "State, reviews and CI of one PR",
    icon: GitPullRequest,
    credential: "github_token",
    defaultSchedule: "1h",
  },
  {
    kind: "github_issue",
    label: "GitHub issue",
    blurb: "State, labels and comments of one issue",
    icon: CircleDot,
    credential: "github_token",
    defaultSchedule: "1h",
  },
  {
    kind: "github_discussion",
    label: "GitHub discussion",
    blurb: "State, answer, labels and comments of one discussion",
    icon: MessagesSquare,
    credential: "github_token",
    defaultSchedule: "1h",
  },
  {
    kind: "gitlab_release",
    label: "GitLab releases",
    blurb: "Releases of a project, any GitLab",
    icon: Tag,
    credential: "gitlab_token",
    defaultSchedule: "15m",
  },
  {
    kind: "gitlab_mr",
    label: "GitLab merge request",
    blurb: "State, approvals and pipeline of one MR",
    icon: GitMerge,
    credential: "gitlab_token",
    defaultSchedule: "15m",
  },
  {
    kind: "oci_tags",
    label: "Image tags",
    blurb: "Tags of an image on any OCI registry",
    icon: Container,
    credential: "registry_basic",
    defaultSchedule: "15m",
  },
];

export function kindInfo(kind: WatchKind): KindInfo {
  return KINDS.find((k) => k.kind === kind) ?? (KINDS[0] as KindInfo);
}

export const CREDENTIAL_KINDS: { kind: CredentialKind; label: string }[] = [
  { kind: "github_token", label: "GitHub token" },
  { kind: "gitlab_token", label: "GitLab token" },
  { kind: "registry_basic", label: "Registry login" },
];

export function credentialLabel(kind: CredentialKind): string {
  return CREDENTIAL_KINDS.find((c) => c.kind === kind)?.label ?? kind;
}

export interface FieldSpec {
  key: string;
  label: string;
  type: "text" | "number" | "checkbox";
  placeholder?: string;
  hint?: string;
  optional?: boolean;
}

const GITHUB_API: FieldSpec = {
  key: "api_url",
  label: "API URL",
  type: "text",
  placeholder: "https://api.github.com",
  hint: "GitHub Enterprise only, e.g. https://ghe.example.com/api/v3",
  optional: true,
};
const REPO: FieldSpec = {
  key: "repo",
  label: "Repository",
  type: "text",
  placeholder: "golang/go",
};
const GITLAB_URL: FieldSpec = {
  key: "base_url",
  label: "GitLab URL",
  type: "text",
  placeholder: "https://gitlab.com",
  hint: "Self-hosted instances: their base URL",
  optional: true,
};
const PROJECT: FieldSpec = {
  key: "project",
  label: "Project path",
  type: "text",
  placeholder: "gitlab-org/gitlab",
};

const ITEM_REQUESTS =
  "Each run then makes 1 more request: GitHub allows 60 an hour without a token.";
const DISCUSSION_REQUESTS =
  "Each run then makes 2 to 3 more requests: GitHub allows 60 an hour without a token.";

// Shared keys (repo, api_url, base_url, project) carry over when the kind changes in the form.
export const FIELDS: Record<WatchKind, FieldSpec[]> = {
  github_repo: [
    REPO,
    { key: "include_prereleases", label: "Include pre-releases", type: "checkbox" },
    { key: "new_issues", label: "Report new issues", type: "checkbox", hint: ITEM_REQUESTS },
    { key: "new_prs", label: "Report new pull requests", type: "checkbox", hint: ITEM_REQUESTS },
    {
      key: "new_discussions",
      label: "Report new discussions",
      type: "checkbox",
      hint: DISCUSSION_REQUESTS,
    },
    GITHUB_API,
  ],
  github_pr: [
    REPO,
    { key: "number", label: "Pull request number", type: "number", placeholder: "123" },
    GITHUB_API,
  ],
  github_issue: [
    REPO,
    { key: "number", label: "Issue number", type: "number", placeholder: "123" },
    GITHUB_API,
  ],
  github_discussion: [
    REPO,
    { key: "number", label: "Discussion number", type: "number", placeholder: "123" },
    GITHUB_API,
  ],
  gitlab_release: [GITLAB_URL, PROJECT],
  gitlab_mr: [
    GITLAB_URL,
    PROJECT,
    { key: "iid", label: "Merge request number", type: "number", placeholder: "42" },
  ],
  oci_tags: [
    {
      key: "registry",
      label: "Registry",
      type: "text",
      placeholder: "docker.io",
      hint: "ghcr.io, quay.io, or your registry's host",
      optional: true,
    },
    {
      key: "repository",
      label: "Repository",
      type: "text",
      placeholder: "library/postgres",
      hint: 'Docker Hub official images can be just "postgres"',
    },
    {
      key: "tag_regex",
      label: "Tag filter",
      type: "text",
      placeholder: "^\\d+\\.\\d+$",
      hint: "Optional regular expression; only matching tags are tracked",
      optional: true,
    },
    { key: "semver_only", label: "Semantic versions only, sorted", type: "checkbox" },
  ],
};

export type FormValues = Record<string, string | boolean>;

/** The config to send for kind: only that kind's fields, trimmed, optional blanks left out. */
export function toConfig(kind: WatchKind, values: FormValues): WatchConfig {
  const out: WatchConfig = {};
  for (const f of FIELDS[kind]) {
    const v = values[f.key];
    if (f.type === "checkbox") {
      out[f.key] = v === true;
      continue;
    }
    const s = typeof v === "string" ? v.trim() : "";
    if (s === "") continue;
    out[f.key] = f.type === "number" ? Number(s) : s;
  }
  return out;
}

export function fromConfig(config: WatchConfig): FormValues {
  const out: FormValues = {};
  for (const [k, v] of Object.entries(config)) {
    out[k] = typeof v === "boolean" ? v : String(v ?? "");
  }
  return out;
}

/** One line naming what the watch follows: "golang/go releases", "group/proj !12", "postgres tags". */
export function targetSummary(w: Pick<Watch, "kind" | "config">): string {
  const c = w.config as Record<string, string | number | boolean | undefined>;
  switch (w.kind) {
    case "github_repo":
      return c.new_issues === true || c.new_prs === true || c.new_discussions === true
        ? String(c.repo)
        : `${c.repo} releases`;
    case "github_pr":
    case "github_issue":
    case "github_discussion":
      return `${c.repo} #${c.number}`;
    case "gitlab_release":
      return `${c.project} releases`;
    case "gitlab_mr":
      return `${c.project} !${c.iid}`;
    case "oci_tags": {
      const repo = String(c.repository ?? "");
      if (c.registry === "docker.io" || !c.registry)
        return `${repo.replace(/^library\//, "")} tags`;
      return `${c.registry}/${repo} tags`;
    }
  }
}

const DEFAULT_TARGET: Record<WatchKind, [field: string, fallback: string]> = {
  github_repo: ["api_url", "https://api.github.com"],
  github_pr: ["api_url", "https://api.github.com"],
  github_issue: ["api_url", "https://api.github.com"],
  github_discussion: ["api_url", "https://api.github.com"],
  gitlab_release: ["base_url", "https://gitlab.com"],
  gitlab_mr: ["base_url", "https://gitlab.com"],
  oci_tags: ["registry", "docker.io"],
};

// Same aliases the server normalizes to docker.io (pkg/watch/config.go ociTags).
const DOCKER_HUB_ALIASES = new Set([
  "index.docker.io",
  "registry-1.docker.io",
  "registry.hub.docker.com",
]);

/** The host an attached credential goes to, from the form values with the server's defaults. */
export function credentialHost(kind: WatchKind, values: FormValues): string {
  const [field, fallback] = DEFAULT_TARGET[kind];
  const raw = typeof values[field] === "string" ? (values[field] as string).trim() : "";
  const v = raw || fallback;
  if (kind === "oci_tags") {
    const host = v.toLowerCase();
    return DOCKER_HUB_ALIASES.has(host) ? "docker.io" : host;
  }
  try {
    return new URL(v).host;
  } catch {
    return v;
  }
}

export const SCHEDULE_PRESETS = [
  { label: "5 min", value: "5m" },
  { label: "15 min", value: "15m" },
  { label: "1 hour", value: "1h" },
  { label: "6 hours", value: "6h" },
  { label: "Daily 09:00", value: "0 9 * * *" },
];

const UNIT_NAMES: Record<string, [string, string]> = {
  h: ["hour", "hours"],
  m: ["minute", "minutes"],
  s: ["second", "seconds"],
};
const DAYS = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"];

function pad(n: string): string {
  return n.padStart(2, "0");
}

function dayList(dow: string): string | null {
  if (dow === "1-5") return "weekdays";
  if (dow === "0,6" || dow === "6,0") return "weekends";
  const parts = dow.split(",");
  if (!parts.every((d) => /^[0-7]$/.test(d))) return null;
  return parts.map((d) => DAYS[Number(d) % 7]).join(", ");
}

/** Human reading of a schedule; null when it is neither a duration nor a cron we can phrase. */
export function describeSchedule(raw: string): string | null {
  const s = raw.trim().split(/\s+/).join(" ");
  if (/^(\d+[hms])+$/.test(s)) {
    const parts = [...s.matchAll(/(\d+)([hms])/g)].map(([, n, u = "m"]) => {
      const [one, many] = UNIT_NAMES[u] ?? ["", ""];
      return n === "1" ? `1 ${one}` : `${n} ${many}`;
    });
    return `Every ${parts.join(" ")}`;
  }
  const f = s.split(" ");
  if (f.length !== 5) return null;
  const [min, hour, dom, mon, dow] = f as [string, string, string, string, string];
  // In range or not phrased: an out-of-range field is the server's 400 to explain, not ours to read.
  const num = (x: string, lo: number, hi: number) =>
    /^\d{1,2}$/.test(x) && Number(x) >= lo && Number(x) <= hi;
  const step = /^\*\/(\d+)$/.exec(min);
  if (step && hour === "*" && dom === "*" && mon === "*" && dow === "*") {
    return `Every ${step[1]} minutes`;
  }
  if (num(min, 0, 59) && hour === "*" && dom === "*" && mon === "*" && dow === "*") {
    return `Every hour at minute ${Number(min)}`;
  }
  if (!num(min, 0, 59) || !num(hour, 0, 23) || mon !== "*") return null;
  const at = `${pad(hour)}:${pad(min)}`;
  if (dom === "*" && dow === "*") return `Every day at ${at}`;
  if (dom === "*") {
    const days = dayList(dow);
    return days ? `At ${at} on ${days}` : null;
  }
  if (num(dom, 1, 31) && dow === "*") return `At ${at} on day ${Number(dom)} of every month`;
  return null;
}

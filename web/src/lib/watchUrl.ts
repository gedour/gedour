import type { WatchConfig, WatchKind } from "@/types/api";

export interface Recognized {
  kind: WatchKind;
  config: WatchConfig;
}

// Registries whose https://<host>/<repo> form is pasted as often as the bare reference.
const REGISTRY_HOSTS = new Set(["ghcr.io", "quay.io", "docker.io", "registry.gitlab.com"]);

// Hosts whose web URLs we read as pages, not as image references.
const WEB_HOSTS = new Set(["github.com", "www.github.com", "gitlab.com", "hub.docker.com"]);

function webURL(input: string): URL | null {
  const withScheme = /^[a-z][a-z0-9+.-]*:\/\//i.test(input) ? input : null;
  const host = input.split("/")[0]?.toLowerCase() ?? "";
  const candidate =
    withScheme ??
    (WEB_HOSTS.has(host) || input.toLowerCase().startsWith("quay.io/repository/")
      ? `https://${input}`
      : null);
  if (!candidate) return null;
  try {
    const u = new URL(candidate);
    return u.protocol === "https:" || u.protocol === "http:" ? u : null;
  } catch {
    return null;
  }
}

/** Decoded path segments, or null when one is not valid percent-encoding (not a URL we can read). */
function segments(u: URL): string[] | null {
  try {
    return u.pathname
      .split("/")
      .filter(Boolean)
      .map((s) => decodeURIComponent(s));
  } catch {
    return null;
  }
}

const positive = (s: string | undefined) => (s && /^\d+$/.test(s) && Number(s) > 0 ? Number(s) : 0);

function github(parts: string[]): Recognized | null {
  // github.com/orgs/<org>/packages/container/package/<img> is a GHCR image page.
  if (parts[0] === "orgs" && parts[2] === "packages" && parts[3] === "container" && parts[5]) {
    return {
      kind: "oci_tags",
      config: { registry: "ghcr.io", repository: `${parts[1]}/${parts[5]}`.toLowerCase() },
    };
  }
  const [owner, name, section, num] = parts;
  if (!owner || !name) return null;
  const repo = `${owner}/${name.replace(/\.git$/, "")}`;
  const number = positive(num);
  if ((section === "pull" || section === "pulls") && number) {
    return { kind: "github_pr", config: { repo, number } };
  }
  return { kind: "github_repo", config: { repo } };
}

function gitlab(u: URL, parts: string[], dotCom: boolean): Recognized | null {
  // Everything before the /-/ route marker is the project path.
  const marker = parts.indexOf("-");
  const project = (marker === -1 ? parts : parts.slice(0, marker)).join("/").replace(/\.git$/, "");
  if (project.split("/").length < 2) return null;
  const config: WatchConfig = { project };
  if (!dotCom) config.base_url = u.origin;
  const route = marker === -1 ? [] : parts.slice(marker + 1);
  const iid = positive(route[1]);
  if (route[0] === "merge_requests" && iid) {
    return { kind: "gitlab_mr", config: { ...config, iid } };
  }
  return { kind: "gitlab_release", config };
}

function dockerHub(parts: string[]): Recognized | null {
  if (parts[0] === "_" && parts[1]) return image("docker.io", `library/${parts[1]}`);
  if ((parts[0] === "r" || parts[0] === "repository") && parts[1] && parts[2]) {
    return image("docker.io", `${parts[1]}/${parts[2]}`);
  }
  return null;
}

function image(registry: string, repository: string): Recognized {
  return { kind: "oci_tags", config: { registry, repository: repository.toLowerCase() } };
}

/** Reads an image reference such as ghcr.io/org/img:tag, registry:5000/a/b or postgres:16. */
function imageRef(input: string): Recognized | null {
  const ref = input.replace(/@sha256:[0-9a-f]+$/i, "");
  const parts = ref.split("/");
  const first = parts[0] ?? "";
  const hasHost =
    parts.length > 1 && (first.includes(".") || first.includes(":") || first === "localhost");
  const path = (hasHost ? parts.slice(1) : parts).join("/");
  // A tag is a ':' after the last '/': registry ports come before it.
  const lastColon = path.lastIndexOf(":");
  const repository = lastColon > path.lastIndexOf("/") ? path.slice(0, lastColon) : path;
  const tagged = lastColon > path.lastIndexOf("/") || input !== ref;
  if (!/^[a-z0-9]+([._-][a-z0-9]+)*(\/[a-z0-9]+([._-][a-z0-9]+)*)*$/i.test(repository)) return null;
  if (hasHost) {
    const registry = first.toLowerCase();
    return image(registry === "index.docker.io" ? "docker.io" : registry, repository);
  }
  // Bare "owner/name" could be a GitHub repo just as well; only a tag makes it an image.
  if (!tagged) return null;
  return image("docker.io", repository.includes("/") ? repository : `library/${repository}`);
}

/** Recognizes a pasted URL or image reference and returns the watch it describes, or null. */
export function recognize(raw: string): Recognized | null {
  const input = raw.trim();
  if (!input || /\s/.test(input)) return null;
  const u = webURL(input);
  if (!u) return imageRef(input);
  const host = u.hostname.toLowerCase().replace(/^www\./, "");
  const parts = segments(u);
  if (!parts) return null;
  if (host === "github.com") return github(parts);
  if (host === "hub.docker.com") return dockerHub(parts);
  if (host === "quay.io" && parts[0] === "repository" && parts[1] && parts[2]) {
    return image("quay.io", `${parts[1]}/${parts[2]}`);
  }
  if (host === "gitlab.com") return gitlab(u, parts, true);
  // Self-hosted GitLab: its /-/ routes, or a host named like one.
  if (parts.includes("-") || host.split(".").includes("gitlab")) return gitlab(u, parts, false);
  if (REGISTRY_HOSTS.has(host)) return imageRef(`${host}/${parts.join("/")}`);
  return null;
}

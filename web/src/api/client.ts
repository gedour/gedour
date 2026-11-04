import type { VersionInfo } from "@/types/api";

async function getJSON<T>(path: string): Promise<T> {
  const res = await fetch(path, { headers: { Accept: "application/json" } });
  if (!res.ok) {
    throw new Error(`${path}: HTTP ${res.status}`);
  }
  return (await res.json()) as T;
}

export function getVersion(): Promise<VersionInfo> {
  return getJSON<VersionInfo>("/api/v1/version");
}

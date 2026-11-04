export type ChangeKind = "added" | "removed" | "changed" | "same";

export interface Change {
  path: string;
  kind: ChangeKind;
  before?: unknown;
  after?: unknown;
}

const isObject = (v: unknown): v is Record<string, unknown> =>
  typeof v === "object" && v !== null && !Array.isArray(v);

// Leaves keyed by dotted path; arrays and scalars are leaves, so a reordered list reads as one change.
function flatten(v: unknown, prefix: string, out: Map<string, unknown>): void {
  if (isObject(v)) {
    for (const [k, child] of Object.entries(v)) flatten(child, prefix ? `${prefix}.${k}` : k, out);
  } else if (v !== undefined && v !== null) {
    out.set(prefix || "(value)", v);
  }
}

/** Per-field diff of two JSON documents (null/undefined = absent), sorted by path. */
export function diffJSON(before: unknown, after: unknown): Change[] {
  const b = new Map<string, unknown>();
  const a = new Map<string, unknown>();
  flatten(before, "", b);
  flatten(after, "", a);
  const paths = [...new Set([...b.keys(), ...a.keys()])].sort();
  return paths.map((path) => {
    if (!b.has(path)) return { path, kind: "added", after: a.get(path) };
    if (!a.has(path)) return { path, kind: "removed", before: b.get(path) };
    const same = JSON.stringify(b.get(path)) === JSON.stringify(a.get(path));
    return { path, kind: same ? "same" : "changed", before: b.get(path), after: a.get(path) };
  });
}

export function formatValue(v: unknown): string {
  return typeof v === "string" ? v : JSON.stringify(v);
}

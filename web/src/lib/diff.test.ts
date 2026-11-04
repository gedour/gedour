import { describe, expect, it } from "vitest";

import { diffJSON, formatValue } from "@/lib/diff";

describe("diffJSON", () => {
  it("marks added, removed, changed and unchanged leaves by dotted path", () => {
    expect(
      diffJSON(
        { name: "a", cfg: { repo: "x/y", tags: [1] }, gone: true },
        { name: "b", cfg: { repo: "x/y", tags: [1, 2] }, extra: null, fresh: 0 },
      ),
    ).toEqual([
      { path: "cfg.repo", kind: "same", before: "x/y", after: "x/y" },
      { path: "cfg.tags", kind: "changed", before: [1], after: [1, 2] },
      { path: "fresh", kind: "added", after: 0 },
      { path: "gone", kind: "removed", before: true },
      { path: "name", kind: "changed", before: "a", after: "b" },
    ]);
  });

  it("handles creates, deletes, scalars and empty documents", () => {
    expect(diffJSON(null, { id: 1 })).toEqual([{ path: "id", kind: "added", after: 1 }]);
    expect(diffJSON({ id: 1 }, undefined)).toEqual([{ path: "id", kind: "removed", before: 1 }]);
    expect(diffJSON("a", "b")).toEqual([
      { path: "(value)", kind: "changed", before: "a", after: "b" },
    ]);
    expect(diffJSON({}, null)).toEqual([]);
  });

  it("formats strings bare and everything else as JSON", () => {
    expect(formatValue("x")).toBe("x");
    expect(formatValue([1, "a"])).toBe('[1,"a"]');
    expect(formatValue(false)).toBe("false");
  });
});

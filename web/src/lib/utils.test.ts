import { describe, expect, it, vi } from "vitest";

import { cn, readStorage, writeStorage } from "@/lib/utils";

describe("storage helpers", () => {
  it("round-trips a value", () => {
    writeStorage("k", "v");
    expect(readStorage("k")).toBe("v");
  });

  it("swallows storage failures", () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("denied");
    });
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("quota");
    });
    expect(() => writeStorage("k", "v")).not.toThrow();
    expect(readStorage("k")).toBeNull();
  });
});

describe("cn", () => {
  it("lets the later Tailwind class win", () => {
    expect(cn("px-2", false, "px-4")).toBe("px-4");
  });
});

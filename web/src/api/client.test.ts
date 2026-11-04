import { describe, expect, it, vi } from "vitest";

import { getVersion } from "@/api/client";

describe("getVersion", () => {
  it("returns the decoded body", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(Response.json({ version: "v1" })));
    await expect(getVersion()).resolves.toEqual({ version: "v1" });
    expect(fetch).toHaveBeenCalledWith("/api/v1/version", expect.anything());
  });

  it("rejects on a non-2xx status", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("", { status: 503 })));
    await expect(getVersion()).rejects.toThrow("HTTP 503");
  });
});

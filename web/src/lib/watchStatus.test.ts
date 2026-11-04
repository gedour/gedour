import { describe, expect, it } from "vitest";

import { nextRunLabel, nextRunText, STATUS, statusOf } from "@/lib/watchStatus";

describe("statusOf", () => {
  it.each([
    [{ paused: true, last_status: "error" as const }, "paused"],
    [{ paused: false, last_status: null }, "pending"],
    [{ paused: false, last_status: "ok" as const }, "ok"],
    [{ paused: false, last_status: "error" as const }, "error"],
    [{ paused: false, last_status: "rate_limited" as const }, "rate_limited"],
  ])("%o is %s", (w, want) => {
    expect(statusOf(w)).toBe(want);
    expect(STATUS[statusOf(w)].label).toBeTruthy();
  });
});

describe("nextRunText", () => {
  const now = new Date("2026-11-04T12:00:00Z");
  it("reads relative, due now, or paused", () => {
    expect(nextRunText({ paused: false, next_run_at: "2026-11-04T12:14:00Z" }, now)).toBe(
      "in 14 minutes",
    );
    expect(nextRunText({ paused: false, next_run_at: "2026-11-04T11:00:00Z" }, now)).toBe(
      "due now",
    );
    expect(nextRunText({ paused: true, next_run_at: "2026-11-04T13:00:00Z" }, now)).toBe("paused");
    expect(nextRunText({ paused: false, next_run_at: "2099-01-01T00:00:00Z" })).toMatch(/^in /);
    // A claimed run shows as running, even on a watch paused meanwhile.
    expect(
      nextRunText({ paused: true, running: true, next_run_at: "2026-11-04T11:00:00Z" }, now),
    ).toBe("running now");
  });

  it("words the list column", () => {
    const at = (next_run_at: string, running = false) =>
      nextRunLabel({ paused: false, running, next_run_at }, now);
    expect(at("2026-11-04T12:14:00Z")).toBe("Next in 14 minutes");
    expect(at("2026-11-04T11:00:00Z")).toBe("Due now");
    expect(at("2026-11-04T11:00:00Z", true)).toBe("Running now");
  });
});

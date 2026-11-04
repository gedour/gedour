import { describe, expect, it } from "vitest";

import { dayStartISO, formatAbsolute, formatRelative } from "@/lib/time";

const now = new Date("2026-11-04T12:00:00Z");
const ago = (secs: number) => new Date(now.getTime() - secs * 1000);

describe("time", () => {
  it.each([
    [5, "just now"],
    [90, "1 minute ago"],
    [3 * 3600, "3 hours ago"],
    [26 * 3600, "yesterday"],
    [10 * 86400, "last week"],
    [400 * 86400, "last year"],
  ])("%ss ago reads %s", (secs, want) => {
    expect(formatRelative(ago(secs), now)).toBe(want);
  });

  it("handles the future and defaults to the current time", () => {
    expect(formatRelative(new Date(now.getTime() + 2 * 3600 * 1000), now)).toBe("in 2 hours");
    expect(formatRelative(new Date())).toBe("just now");
  });

  it("formats an absolute date with the year", () => {
    expect(formatAbsolute(now)).toMatch(/2026/);
  });

  it("turns a local day into its start instant, or the next day's", () => {
    expect(new Date(dayStartISO("2026-03-01")).getDate()).toBe(1);
    expect(new Date(dayStartISO("2026-03-01")).getHours()).toBe(0);
    const end = new Date(dayStartISO("2026-02-28", true));
    expect([end.getMonth(), end.getDate(), end.getHours()]).toEqual([2, 1, 0]);
  });
});

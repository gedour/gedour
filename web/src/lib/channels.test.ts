import { afterEach, describe, expect, it, vi } from "vitest";

import {
  browserTimeZone,
  channelKind,
  channelTarget,
  digestErrors,
  digestSummary,
  EVERY_DAY,
  timeZones,
  type Weekday,
} from "@/lib/channels";

describe("channelTarget", () => {
  it("names where each type sends", () => {
    expect(channelTarget({ type: "telegram", config: { chat_id: "-1001" } })).toBe("chat -1001");
    expect(channelTarget({ type: "email", config: { to: ["a@x", "b@x"] } })).toBe("a@x, b@x");
    expect(channelTarget({ type: "email", config: {} })).toBe("");
    expect(channelTarget({ type: "webhook", config: { url: "https://h.example:8443/p?q" } })).toBe(
      "h.example:8443",
    );
    expect(channelTarget({ type: "webhook", config: { url: "not a url" } })).toBe("not a url");
    expect(channelTarget({ type: "webhook", config: {} })).toBe("");
    expect(channelTarget({ type: "script", config: { script_id: "s1" } })).toBe("runs a script");
    expect(
      channelTarget({ type: "script", config: { script_id: "s1" } }, new Map([["s1", "Notify"]])),
    ).toBe("runs Notify");
    expect(channelKind("email").label).toBe("Email");
    // 003-AC1
    expect(
      channelTarget({
        type: "nextcloud_talk",
        config: { url: "https://cloud.example.com/nc", conversation: "abcd1234" },
      }),
    ).toBe("conversation abcd1234 on cloud.example.com");
    expect(channelKind("nextcloud_talk").label).toBe("Nextcloud Talk");
    expect(channelKind("script").label).toBe("Script");
    expect(channelKind("nope" as "email").label).toBe("Webhook");
  });
});

describe("digest helpers", () => {
  afterEach(() => vi.restoreAllMocks());

  // 012-AC13: a digest rule's summary names its times and zone; immediate has none.
  it("summarizes a digest schedule", () => {
    const at = (...times: string[]) => times.map((time) => ({ time, days: EVERY_DAY }));
    expect(
      digestSummary({ delivery: "digest", schedule: at("09:00", "18:00"), tz: "Europe/Paris" }),
    ).toBe("Digest at 09:00, 18:00 (Europe/Paris)");
    expect(digestSummary({ delivery: "digest", schedule: at("07:30"), tz: "" })).toBe(
      "Digest at 07:30 (UTC)",
    );
    expect(digestSummary({ delivery: "immediate", schedule: [], tz: "" })).toBeNull();
    expect(digestSummary({ delivery: "digest", schedule: null, tz: "UTC" })).toBe("Digest (UTC)");
    expect(digestSummary({ delivery: "digest", schedule: [], tz: "Europe/Paris" })).toBe(
      "Digest (Europe/Paris)",
    );
    // A pre-018 server's rule: no schedule field at all.
    const old = { delivery: "digest", tz: "" } as Parameters<typeof digestSummary>[0];
    expect(digestSummary(old)).toBe("Digest (UTC)");
  });

  // 018-AC12: days a time does not run on every day of, in runs: 3+ days collapse, 2 are listed, no wrap past Sunday.
  it("names the days of a digest time", () => {
    const one = (days: Weekday[] | null) =>
      digestSummary({ delivery: "digest", schedule: [{ time: "09:00", days }], tz: "UTC" });
    expect(one(EVERY_DAY)).toBe("Digest at 09:00 (UTC)");
    expect(one(null)).toBe("Digest at 09:00 (UTC)");
    expect(one(["mon", "tue", "wed", "thu", "fri"])).toBe("Digest at 09:00 Mon–Fri (UTC)");
    expect(one(["sat"])).toBe("Digest at 09:00 Sat (UTC)");
    expect(one(["sat", "sun"])).toBe("Digest at 09:00 Sat, Sun (UTC)");
    expect(one(["mon", "wed", "fri"])).toBe("Digest at 09:00 Mon, Wed, Fri (UTC)");
    expect(one(["mon", "tue", "wed", "fri"])).toBe("Digest at 09:00 Mon–Wed, Fri (UTC)");
    expect(one(["mon", "sat", "sun"])).toBe("Digest at 09:00 Mon, Sat, Sun (UTC)");
    expect(one(["tue", "wed", "thu", "fri", "sat", "sun"])).toBe("Digest at 09:00 Tue–Sun (UTC)");
    expect(
      digestSummary({
        delivery: "digest",
        schedule: [
          { time: "09:00", days: ["mon", "tue", "wed", "thu", "fri"] },
          { time: "18:00", days: ["sat", "sun"] },
        ],
        tz: "Europe/Paris",
      }),
    ).toBe("Digest at 09:00 Mon–Fri, 18:00 Sat, Sun (Europe/Paris)");
  });

  // 012-AC11, 018-AC11: the form refuses what the server refuses (012-AC1, 018-AC3), keyed like its field errors.
  it("mirrors the server's schedule checks", () => {
    const at = (...times: string[]) => times.map((time) => ({ time, days: EVERY_DAY }));
    expect(digestErrors(at("09:00", "18:00"), "Europe/Paris")).toEqual({});
    expect(digestErrors(at("09:00"), "")).toEqual({});
    expect(digestErrors([], "UTC")).toEqual({ schedule: "at least one time of day" });
    expect(digestErrors(at("24:00", "9:00", "", "09:00", "09:00"), "UTC")).toEqual({
      "schedule[0].time": "a local time as HH:MM, e.g. 09:00",
      "schedule[1].time": "a local time as HH:MM, e.g. 09:00",
      "schedule[2].time": "a local time as HH:MM, e.g. 09:00",
      "schedule[4].time": "09:00 is already listed",
    });
    expect(
      digestErrors(
        [
          { time: "09:00", days: ["mon"] },
          { time: "", days: [] },
        ],
        "UTC",
      ),
    ).toEqual({
      "schedule[1].time": "a local time as HH:MM, e.g. 09:00",
      "schedule[1].days": "at least one day",
    });
    const many = at(
      ...Array.from(
        { length: 25 },
        (_, i) => `${String(i % 24).padStart(2, "0")}:0${i >= 24 ? 1 : 0}`,
      ),
    );
    expect(digestErrors(many, "UTC")).toEqual({ schedule: "at most 24 times" });
    expect(digestErrors(many.slice(0, 24), "UTC")).toEqual({});
    // Valid in Go though Intl renames them.
    for (const tz of ["Asia/Kolkata", "Etc/UTC", "UTC", "EST5EDT"])
      expect(digestErrors([{ time: "09:00", days: ["sun"] }], tz)).toEqual({});
    for (const tz of [
      "Local",
      "Mars/Olympus",
      "not a zone",
      "europe/paris",
      "+01:00",
      "-05:00",
      "asia/kolkata",
      "etc/utc",
    ])
      expect(digestErrors([{ time: "09:00", days: ["sun"] }], tz)).toEqual({
        tz: "an IANA time zone, e.g. Europe/Paris",
      });
  });

  it("defaults to the browser's zone and lists zones where it can", () => {
    expect(browserTimeZone()).toBe(Intl.DateTimeFormat().resolvedOptions().timeZone);
    vi.spyOn(Intl.DateTimeFormat.prototype, "resolvedOptions").mockReturnValue({
      timeZone: "",
    } as Intl.ResolvedDateTimeFormatOptions);
    expect(browserTimeZone()).toBe("UTC");
    const zones = timeZones();
    expect(zones).toContain("Europe/Paris");
    expect(zones[0]).toBe("UTC");
    expect(zones.filter((z) => z === "UTC")).toHaveLength(1);
    const supported = Intl.supportedValuesOf;
    Object.defineProperty(Intl, "supportedValuesOf", { value: undefined, configurable: true });
    try {
      expect(timeZones()).toEqual(["UTC"]);
      Object.defineProperty(Intl, "supportedValuesOf", {
        value: () => ["Europe/Paris", "UTC"],
        configurable: true,
      });
      expect(timeZones()).toEqual(["UTC", "Europe/Paris"]);
    } finally {
      Object.defineProperty(Intl, "supportedValuesOf", { value: supported, configurable: true });
    }
  });
});

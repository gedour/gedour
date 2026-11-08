import { afterEach, describe, expect, it, vi } from "vitest";

import {
  browserTimeZone,
  channelKind,
  channelTarget,
  digestErrors,
  digestSummary,
  timeZones,
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
    expect(digestSummary({ delivery: "digest", at: ["09:00", "18:00"], tz: "Europe/Paris" })).toBe(
      "Digest at 09:00, 18:00 (Europe/Paris)",
    );
    expect(digestSummary({ delivery: "digest", at: ["07:30"], tz: "" })).toBe(
      "Digest at 07:30 (UTC)",
    );
    expect(digestSummary({ delivery: "immediate", at: [], tz: "" })).toBeNull();
    expect(digestSummary({ delivery: "digest", at: [], tz: "UTC" })).toBeNull();
  });

  // 012-AC11: the form refuses what the server refuses (012-AC1), keyed like its field errors.
  it("mirrors the server's schedule checks", () => {
    expect(digestErrors(["09:00", "18:00"], "Europe/Paris")).toEqual({});
    expect(digestErrors(["09:00"], "")).toEqual({});
    expect(digestErrors([], "UTC")).toEqual({ at: "at least one time of day" });
    expect(digestErrors(["24:00", "9:00", "", "09:00", "09:00"], "UTC")).toEqual({
      "at[0]": "a local time as HH:MM, e.g. 09:00",
      "at[1]": "a local time as HH:MM, e.g. 09:00",
      "at[2]": "a local time as HH:MM, e.g. 09:00",
      "at[4]": "09:00 is already listed",
    });
    const many = Array.from(
      { length: 25 },
      (_, i) => `${String(i % 24).padStart(2, "0")}:0${i >= 24 ? 1 : 0}`,
    );
    expect(digestErrors(many, "UTC")).toEqual({ at: "at most 24 times" });
    expect(digestErrors(many.slice(0, 24), "UTC")).toEqual({});
    // Valid in Go though Intl renames them.
    for (const tz of ["Asia/Kolkata", "Etc/UTC", "UTC", "EST5EDT"])
      expect(digestErrors(["09:00"], tz)).toEqual({});
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
      expect(digestErrors(["09:00"], tz)).toEqual({ tz: "an IANA time zone, e.g. Europe/Paris" });
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

import { describe, expect, it } from "vitest";

import { channelKind, channelTarget } from "@/lib/channels";

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

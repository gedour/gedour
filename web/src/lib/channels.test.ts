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
    expect(channelKind("email").label).toBe("Email");
    expect(channelKind("nope" as "email").label).toBe("Webhook");
  });
});

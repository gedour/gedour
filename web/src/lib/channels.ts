import { FileCode2, Mail, Send, Webhook, type LucideIcon } from "lucide-react";

import type { Channel, ChannelKind } from "@/types/api";

export const CHANNEL_KINDS: { type: ChannelKind; label: string; icon: LucideIcon }[] = [
  { type: "telegram", label: "Telegram", icon: Send },
  { type: "email", label: "Email", icon: Mail },
  { type: "webhook", label: "Webhook", icon: Webhook },
  { type: "script", label: "Script", icon: FileCode2 },
];

export function channelKind(type: ChannelKind) {
  return (
    CHANNEL_KINDS.find((k) => k.type === type) ??
    (CHANNEL_KINDS[2] as (typeof CHANNEL_KINDS)[number])
  );
}

/** Where a channel sends: the chat, the recipients, the webhook's host, or the script it runs (named when scripts knows it). */
export function channelTarget(
  c: Pick<Channel, "type" | "config">,
  scripts?: Map<string, string>,
): string {
  const cfg = c.config;
  switch (c.type) {
    case "script":
      return `runs ${scripts?.get(String(cfg.script_id)) ?? "a script"}`;
    case "telegram":
      return `chat ${String(cfg.chat_id ?? "")}`;
    case "email":
      return Array.isArray(cfg.to) ? cfg.to.join(", ") : "";
    default:
      try {
        return new URL(String(cfg.url)).host;
      } catch {
        return String(cfg.url ?? "");
      }
  }
}

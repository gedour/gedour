import {
  BellRing,
  FileCode2,
  Mail,
  MessagesSquare,
  Send,
  Webhook,
  type LucideIcon,
} from "lucide-react";

import type { Channel, ChannelKind } from "@/types/api";

export const CHANNEL_KINDS: { type: ChannelKind; label: string; icon: LucideIcon }[] = [
  { type: "telegram", label: "Telegram", icon: Send },
  { type: "nextcloud_talk", label: "Nextcloud Talk", icon: MessagesSquare },
  { type: "email", label: "Email", icon: Mail },
  { type: "webhook", label: "Webhook", icon: Webhook },
  { type: "script", label: "Script", icon: FileCode2 },
  { type: "web_push", label: "Web Push", icon: BellRing },
];

export function channelKind(type: ChannelKind) {
  return (
    CHANNEL_KINDS.find((k) => k.type === type) ??
    (CHANNEL_KINDS[3] as (typeof CHANNEL_KINDS)[number])
  );
}

function hostOf(url: unknown): string {
  try {
    return new URL(String(url)).host;
  } catch {
    return String(url ?? "");
  }
}

/** Where a channel sends: the chat, the conversation, the recipients, the webhook's host, or the script it runs (named when scripts knows it). */
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
    case "web_push":
      return Array.isArray(cfg.devices)
        ? `${cfg.devices.length} device${cfg.devices.length === 1 ? "" : "s"}`
        : "all devices";
    case "nextcloud_talk":
      return `conversation ${String(cfg.conversation ?? "")} on ${hostOf(cfg.url)}`;
    default:
      return hostOf(cfg.url);
  }
}

import {
  BellRing,
  FileCode2,
  Mail,
  MessagesSquare,
  Send,
  Webhook,
  type LucideIcon,
} from "lucide-react";

import type { Channel, ChannelKind, DeliveryStatus } from "@/types/api";

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

/** Types whose message can list several events (012). */
export const DIGEST_TYPES: ChannelKind[] = ["telegram", "nextcloud_talk", "email", "web_push"];
export const MAX_DIGEST_TIMES = 24;

/** "Digest at 09:00, 18:00 (Europe/Paris)" for a digest channel, null for an immediate one. */
export function digestSummary(cfg: Channel["config"]): string | null {
  if (cfg.delivery !== "digest" || !Array.isArray(cfg.at)) return null;
  return `Digest at ${cfg.at.join(", ")} (${String(cfg.tz ?? "UTC")})`;
}

/** The browser's zone, the form's default for a new digest. */
export function browserTimeZone(): string {
  return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
}

/** Zone names for the time zone suggestions, UTC first; engines leave it out of their list. */
export function timeZones(): string[] {
  const list =
    typeof Intl.supportedValuesOf === "function" ? Intl.supportedValuesOf("timeZone") : [];
  return ["UTC", ...list.filter((z) => z !== "UTC")];
}

// Intl accepts offsets and any case, Go's time.LoadLocation neither. An alias Intl renames
// (Asia/Kolkata -> Asia/Calcutta) is still a valid IANA name, so only a case-only rename refuses;
// every IANA segment starts uppercase, which also catches a lowercase alias (asia/kolkata).
function knownTimeZone(tz: string): boolean {
  if (/^[+-]/.test(tz) || !tz.split("/").every((seg) => /^[A-Z]/.test(seg))) return false;
  try {
    const resolved = new Intl.DateTimeFormat("en", { timeZone: tz }).resolvedOptions().timeZone;
    return resolved === tz || resolved.toLowerCase() !== tz.toLowerCase();
  } catch {
    return false;
  }
}

/** The server's digest checks (pkg/route/digest.go), plus duplicate times; keyed like its field errors, empty tz means UTC. */
export function digestErrors(at: string[], tz: string): Record<string, string> {
  const out: Record<string, string> = {};
  at.forEach((v, i) => {
    if (!/^([01][0-9]|2[0-3]):[0-5][0-9]$/.test(v))
      out[`at[${i}]`] = "a local time as HH:MM, e.g. 09:00";
    else if (at.indexOf(v) < i) out[`at[${i}]`] = `${v} is already listed`;
  });
  if (at.length === 0) out.at = "at least one time of day";
  else if (at.length > MAX_DIGEST_TIMES) out.at = `at most ${MAX_DIGEST_TIMES} times`;
  if (tz && (tz === "Local" || !knownTimeZone(tz))) out.tz = "an IANA time zone, e.g. Europe/Paris";
  return out;
}

/** Delivery status labels and badge colours, in the order the status filter lists them. */
export const DELIVERY_STATUS: Record<DeliveryStatus, { label: string; className: string }> = {
  pending: {
    label: "Pending",
    className: "bg-sky-500/10 text-sky-800 ring-sky-600/25 dark:text-sky-300",
  },
  sent: {
    label: "Sent",
    className: "bg-emerald-500/10 text-emerald-800 ring-emerald-600/25 dark:text-emerald-300",
  },
  failed: {
    label: "Retrying",
    className: "bg-amber-500/10 text-amber-800 ring-amber-600/25 dark:text-amber-300",
  },
  dead: {
    label: "Failed",
    className: "bg-rose-500/10 text-rose-800 ring-rose-600/25 dark:text-rose-300",
  },
  skipped: {
    label: "Skipped",
    className: "bg-zinc-500/10 text-zinc-700 ring-zinc-600/25 dark:text-zinc-300",
  },
};

import { cn } from "@/lib/utils";
import { channelKind } from "@/lib/channels";
import type { ChannelKind, DeliveryStatus } from "@/types/api";

export function ChannelIcon({ type, className }: { type: ChannelKind; className?: string }) {
  const Icon = channelKind(type).icon;
  return <Icon aria-hidden className={cn("size-4", className)} />;
}

const STATUS: Record<DeliveryStatus, { label: string; className: string }> = {
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

export function DeliveryBadge({ status }: { status: DeliveryStatus }) {
  const s = STATUS[status];
  return (
    <span
      className={cn(
        "inline-flex items-center rounded-md px-2 py-0.5 text-xs font-medium ring-1 ring-inset",
        s.className,
      )}
    >
      {s.label}
    </span>
  );
}

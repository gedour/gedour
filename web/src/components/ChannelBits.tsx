import { cn } from "@/lib/utils";
import { channelKind, DELIVERY_STATUS } from "@/lib/channels";
import type { ChannelKind, DeliveryStatus } from "@/types/api";

export function ChannelIcon({ type, className }: { type: ChannelKind; className?: string }) {
  const Icon = channelKind(type).icon;
  return <Icon aria-hidden className={cn("size-4", className)} />;
}

export function DeliveryBadge({ status }: { status: DeliveryStatus }) {
  const s = DELIVERY_STATUS[status];
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

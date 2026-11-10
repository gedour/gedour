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

/** A script delivery's log lines, folded. */
export function DeliveryLogs({ logs }: { logs: string }) {
  const lines = logs ? logs.split("\n") : [];
  return (
    <details className="mb-1">
      <summary className="cursor-pointer text-xs text-muted marker:text-muted">
        Logs · {lines.length} line{lines.length === 1 ? "" : "s"}
      </summary>
      <pre className="mt-2 max-h-48 overflow-auto rounded-lg bg-bg p-2 font-mono text-xs break-words whitespace-pre-wrap">
        {logs || "No log line."}
      </pre>
    </details>
  );
}

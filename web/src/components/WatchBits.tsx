import { Pause } from "lucide-react";

import { kindInfo } from "@/lib/watchKinds";
import { STATUS, statusOf } from "@/lib/watchStatus";
import { cn } from "@/lib/utils";
import type { Watch, WatchKind } from "@/types/api";

const badge =
  "inline-flex items-center gap-1 rounded-md px-2 py-0.5 text-xs font-medium ring-1 ring-inset";

export function KindIcon({ kind, className }: { kind: WatchKind; className?: string }) {
  const { icon: Icon } = kindInfo(kind);
  return (
    <span
      aria-hidden
      className={cn(
        "grid size-10 shrink-0 place-items-center rounded-lg bg-beacon-soft text-ink ring-1 ring-beacon/25 ring-inset",
        className,
      )}
    >
      <Icon className="size-5" />
    </span>
  );
}

export function PausedBadge() {
  return (
    <span className={cn(badge, "bg-line/60 text-muted ring-line")}>
      <Pause aria-hidden className="size-3" />
      Paused
    </span>
  );
}

/** What a delete takes with it, for everyone: the cascades of migrations 00004 and 00005. */
export function DeleteWatchWarning() {
  return (
    <p className="text-sm text-muted">
      Deleting a watch also removes its events and every rule on it. Every inbox item and delivery
      for those events goes too. This cannot be undone.
    </p>
  );
}

/** Coloured dot for the run status; the label is always in the accessible name, visible on demand. */
export function StatusDot({
  watch,
  showLabel = false,
}: {
  watch: Pick<Watch, "paused" | "last_status">;
  showLabel?: boolean;
}) {
  const s = STATUS[statusOf(watch)];
  return (
    <span className={cn("inline-flex items-center gap-1.5 text-xs font-medium", s.text)}>
      <span aria-hidden className={cn("size-2.5 shrink-0 rounded-full", s.dot)} />
      <span className={showLabel ? undefined : "sr-only"}>{s.label}</span>
    </span>
  );
}

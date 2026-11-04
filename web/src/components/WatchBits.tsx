import { Pause, Users } from "lucide-react";

import { kindInfo, PERMISSION_LABEL } from "@/lib/watchKinds";
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

/** "Shared by Alice · ro" for watches the caller does not own; nothing for their own. */
export function SharedBadge({ watch }: { watch: Pick<Watch, "permission" | "owner_name"> }) {
  if (watch.permission === "owner") return null;
  return (
    <span
      className={cn(badge, "bg-sky-500/10 text-sky-800 ring-sky-600/25 dark:text-sky-300")}
      title={`Shared with you, ${PERMISSION_LABEL[watch.permission]}`}
    >
      <Users aria-hidden className="size-3" />
      Shared by {watch.owner_name || "someone"} ({watch.permission})
    </span>
  );
}

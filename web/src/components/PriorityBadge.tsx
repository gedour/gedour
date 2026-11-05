import { Flag } from "lucide-react";

import { TONE_CLASS } from "@/lib/events";
import { cn } from "@/lib/utils";

/** Marks a high priority rule or inbox item, by text as well as color. */
export function PriorityBadge() {
  return (
    <span
      className={cn(
        "inline-flex w-fit shrink-0 items-center gap-1 rounded px-1.5 py-px text-xs whitespace-nowrap ring-1 ring-inset",
        TONE_CLASS.bad,
      )}
    >
      <Flag aria-hidden className="size-3" />
      High priority
    </span>
  );
}

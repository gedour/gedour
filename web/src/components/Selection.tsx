import { Check } from "lucide-react";
import type { ReactNode } from "react";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

/**
 * A row's leading icon that is also its selection checkbox: the check shows on hover or keyboard
 * focus, and on every row once one is selected.
 */
export function SelectTile({
  label,
  selected,
  selecting,
  onToggle,
  children,
  className,
}: {
  label: string;
  selected: boolean;
  /** Some row is selected. */
  selecting: boolean;
  onToggle: () => void;
  children: ReactNode;
  className?: string;
}) {
  return (
    <button
      type="button"
      role="checkbox"
      aria-checked={selected}
      aria-label={`Select: ${label}`}
      onClick={onToggle}
      className={cn(
        "group/tile relative grid size-10 shrink-0 place-items-center rounded-lg",
        className,
      )}
    >
      {children}
      <span
        aria-hidden
        className={cn(
          "absolute inset-0 grid place-items-center rounded-lg border-2 opacity-0 transition-opacity",
          selected ? "border-beacon bg-beacon text-surface" : "border-line bg-surface text-muted",
          (selected || selecting) && "opacity-100",
          "group-hover/tile:opacity-100 group-focus-visible/tile:opacity-100",
        )}
      >
        <Check className={cn("size-5", !selected && "opacity-40")} />
      </span>
    </button>
  );
}

/** Above a selectable list: its count, or once rows are selected, their actions. */
export function SelectionBar({
  count,
  selected,
  total,
  onSelect,
  children,
}: {
  /** Shown while nothing is selected, as "3 watches". */
  count: string;
  selected: number;
  total: number;
  /** Replaces the selection: every row, or none. */
  onSelect: (all: boolean) => void;
  /** The bulk actions. */
  children: ReactNode;
}) {
  if (selected === 0) {
    return (
      <div className="flex min-h-9 items-center">
        <span className="text-sm text-muted">{count}</span>
      </div>
    );
  }
  const all = selected === total;
  return (
    <div
      role="toolbar"
      aria-label="Bulk actions"
      className="flex min-h-9 flex-wrap items-center gap-2 rounded-lg border border-beacon/40 bg-beacon-soft/40 px-3 py-1"
    >
      <span className="mr-2 text-sm font-medium">{selected} selected</span>
      {children}
      <Button variant="ghost" size="sm" onClick={() => onSelect(!all)}>
        {all ? "Select none" : `Select all ${total}`}
      </Button>
      <Button variant="ghost" size="sm" className="ml-auto" onClick={() => onSelect(false)}>
        Clear
      </Button>
    </div>
  );
}

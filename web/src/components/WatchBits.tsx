import { Pause, Search } from "lucide-react";
import { createContext, use, useId, type ReactNode } from "react";

import { inputClass } from "@/components/ui/field";
import { KINDS, kindInfo } from "@/lib/watchKinds";
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

/** What a delete takes with it, for everyone: the cascades of migrations 00004, 00005 and 00025. */
export function DeleteWatchWarning() {
  return (
    <p className="text-sm text-muted">
      Deleting a watch also removes its events and takes it out of every rule; a rule left with no
      watch goes too. Every inbox item and delivery for those events goes too. This cannot be
      undone.
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

// The search term, marked in the name and target a row shows. The server also matches the whole
// config, so a row can match with no mark.
const Term = createContext("");

export function SearchTerm({ value, children }: { value: string; children: ReactNode }) {
  return <Term value={value}>{children}</Term>;
}

export function Highlight({ text }: { text: string }) {
  const term = use(Term).toLowerCase();
  if (!term) return text;
  const lower = text.toLowerCase();
  // Some characters change length when lowercased (İ): offsets would no longer line up.
  if (lower.length !== text.length) return text;
  const parts = [];
  let at = 0;
  for (let i = lower.indexOf(term); i !== -1; i = lower.indexOf(term, at)) {
    parts.push(
      text.slice(at, i),
      <mark key={i} className="rounded-sm bg-beacon-soft text-ink ring-1 ring-beacon/40">
        {text.slice(i, i + term.length)}
      </mark>,
    );
    at = i + term.length;
  }
  parts.push(text.slice(at));
  return parts;
}

/** The search box and kind select of the watch filters; the caller wraps them in its search landmark. */
export function WatchSearchFields({
  q,
  setQ,
  kind,
  setKind,
  disabled,
}: {
  q: string;
  setQ: (v: string) => void;
  kind: WatchKind | "";
  setKind: (v: WatchKind | "") => void;
  disabled?: boolean;
}) {
  const ids = { q: useId(), kind: useId() };
  return (
    <>
      <div className="flex min-w-56 flex-1 flex-col gap-1 sm:max-w-80">
        <label htmlFor={ids.q} className="text-xs font-medium text-muted">
          Search
        </label>
        <div className="relative">
          <Search
            aria-hidden
            className="pointer-events-none absolute top-2.5 left-2.5 size-4 text-muted"
          />
          <input
            id={ids.q}
            type="search"
            value={q}
            disabled={disabled}
            placeholder="Name, repository, image…"
            onChange={(e) => setQ(e.target.value)}
            className={cn(inputClass, "pl-8")}
          />
        </div>
      </div>
      <div className="flex flex-col gap-1">
        <label htmlFor={ids.kind} className="text-xs font-medium text-muted">
          Kind
        </label>
        <select
          id={ids.kind}
          value={kind}
          disabled={disabled}
          onChange={(e) => setKind(e.target.value as WatchKind | "")}
          className={cn(inputClass, "w-auto pr-8")}
        >
          <option value="">All kinds</option>
          {KINDS.map((k) => (
            <option key={k.kind} value={k.kind}>
              {k.label}
            </option>
          ))}
        </select>
      </div>
    </>
  );
}

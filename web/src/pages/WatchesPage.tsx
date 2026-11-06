import { keepPreviousData, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Check, Clock, Play, Plus, Search, Trash2 } from "lucide-react";
import { createContext, use, useId, useState } from "react";
import { Link } from "react-router";

import { deleteWatch, listWatches, runAllWatches } from "@/api/client";
import { EmptyState } from "@/components/EmptyState";
import { ErrorState } from "@/components/ErrorState";
import { Button } from "@/components/ui/button";
import { Dialog, DialogActions } from "@/components/ui/dialog";
import { inputClass } from "@/components/ui/field";
import { KindIcon, PausedBadge, SharedBadge, StatusDot } from "@/components/WatchBits";
import { useToast } from "@/hooks/toast";
import { problemText } from "@/lib/problems";
import { describeSchedule, KINDS, targetSummary } from "@/lib/watchKinds";
import { nextRunLabel } from "@/lib/watchStatus";
import { cn } from "@/lib/utils";
import type { NavItem } from "@/nav";
import type { SharedFilter, Watch, WatchKind } from "@/types/api";

// The search term, marked in each row where the server's case-insensitive substring match hit it.
const SearchTerm = createContext("");

function Highlight({ text }: { text: string }) {
  const term = use(SearchTerm).toLowerCase();
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

const SHARED: { value: SharedFilter; label: string }[] = [
  { value: "all", label: "All" },
  { value: "mine", label: "Mine" },
  { value: "with_me", label: "Shared with me" },
];

export function NewWatchButton() {
  return (
    <Button variant="primary" asChild>
      <Link to="/watches/new">
        <Plus aria-hidden />
        New watch
      </Link>
    </Button>
  );
}

function RunAllButton() {
  const client = useQueryClient();
  const toast = useToast();
  const run = useMutation({
    mutationFn: runAllWatches,
    onSuccess: ({ scheduled }) => {
      toast(
        scheduled === 0
          ? "No watch to run"
          : `${scheduled} watch${scheduled === 1 ? "" : "es"} scheduled to run`,
      );
      void client.invalidateQueries({ queryKey: ["watches"] });
    },
    onError: (err) => toast(`Could not run the watches: ${problemText(err)}`, "error"),
  });
  return (
    <Button
      variant="outline"
      disabled={run.isPending}
      title="Run every watch you can run now; paused ones are skipped"
      onClick={() => run.mutate()}
    >
      <Play aria-hidden className={cn(run.isPending && "animate-pulse")} />
      {run.isPending ? "Scheduling…" : "Run all"}
    </Button>
  );
}

function DeleteSelected({
  watches,
  onDone,
}: {
  watches: Watch[];
  onDone: (failed: Set<string>) => void;
}) {
  const [open, setOpen] = useState(false);
  const client = useQueryClient();
  const toast = useToast();
  const n = watches.length;
  const del = useMutation({
    // No bulk endpoint: one DELETE per watch, a failure leaves the others deleted.
    mutationFn: () =>
      Promise.all(
        watches.map((w) =>
          deleteWatch(w.id).then(
            () => ({ id: w.id, error: undefined as unknown }),
            (error: unknown) => ({ id: w.id, error }),
          ),
        ),
      ),
    onSuccess: (results) => {
      const failed = new Set<string>();
      let reason: unknown;
      for (const { id, error } of results) {
        if (error === undefined) {
          client.removeQueries({ queryKey: ["watch", id] });
          continue;
        }
        failed.add(id);
        reason ??= error;
      }
      void client.invalidateQueries({ queryKey: ["watches"] });
      setOpen(false);
      onDone(failed);
      const sent = results.length;
      if (failed.size === 0) toast(`Deleted ${sent} watch${sent === 1 ? "" : "es"}`);
      else toast(`Could not delete ${failed.size} of ${sent}: ${problemText(reason)}`, "error");
    },
  });
  return (
    <>
      <Button variant="ghost" size="sm" onClick={() => setOpen(true)}>
        <Trash2 aria-hidden />
        Delete
      </Button>
      <Dialog
        open={open}
        title={`Delete ${n} watch${n === 1 ? "" : "es"}?`}
        onClose={() => setOpen(false)}
      >
        <p className="text-sm text-muted">
          gedour stops following them and drops their shares. This cannot be undone.
        </p>
        <ul className="max-h-48 list-inside list-disc overflow-y-auto text-sm">
          {watches.map((w) => (
            <li key={w.id}>{w.name}</li>
          ))}
        </ul>
        <DialogActions>
          <Button variant="ghost" onClick={() => setOpen(false)} autoFocus>
            Cancel
          </Button>
          <Button variant="danger" disabled={del.isPending} onClick={() => del.mutate()}>
            {del.isPending ? "Deleting…" : "Delete"}
          </Button>
        </DialogActions>
      </Dialog>
    </>
  );
}

export function WatchesPage({ item }: { item: NavItem }) {
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [q, setQ] = useState("");
  const [kind, setKind] = useState<WatchKind | "">("");
  const [shared, setShared] = useState<SharedFilter>("all");
  const ids = { q: useId(), kind: useId() };
  const filters = { q: q.trim() || undefined, kind: kind || undefined, shared };
  const filtered = Boolean(filters.q || filters.kind || shared !== "all");
  const query = useQuery({
    queryKey: ["watches", filters],
    queryFn: () => listWatches(filters),
    placeholderData: keepPreviousData,
    // Keeps run statuses and "next in …" current while the list stays open.
    refetchInterval: 30_000,
  });
  const now = new Date();
  // Only owners can delete; ids hidden by a filter stay selected but out of the action.
  const owned = (query.data ?? []).filter((w) => w.permission === "owner");
  const chosen = owned.filter((w) => selected.has(w.id));
  const allSelected = owned.length > 0 && chosen.length === owned.length;
  const toggle = (id: string) =>
    setSelected((s) => {
      const next = new Set(s);
      if (!next.delete(id)) next.add(id);
      return next;
    });
  const clear = () => {
    setQ("");
    setKind("");
    setShared("all");
  };

  return (
    <SearchTerm value={filters.q ?? ""}>
      <div className="mx-auto flex w-full max-w-6xl flex-col gap-6 px-5 py-8 md:px-10 md:py-12">
        <title>{`${item.label} – gedour`}</title>
        <div className="flex flex-wrap items-end justify-between gap-4">
          <div className="flex flex-col gap-2">
            <h1 className="font-display text-3xl font-semibold tracking-tight">{item.label}</h1>
            <p className="max-w-prose text-muted">
              Things gedour follows upstream for you, and those others share with you.
            </p>
          </div>
          <div className="flex flex-wrap gap-2">
            <RunAllButton />
            <NewWatchButton />
          </div>
        </div>

        <form
          role="search"
          aria-label="Filter watches"
          className="flex flex-wrap items-end gap-3"
          onSubmit={(e) => e.preventDefault()}
        >
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
          <div
            role="group"
            aria-label="Ownership"
            className="inline-flex h-9 rounded-md border border-line bg-surface p-0.5 shadow-xs"
          >
            {SHARED.map((s) => (
              <button
                key={s.value}
                type="button"
                aria-pressed={shared === s.value}
                onClick={() => setShared(s.value)}
                className={cn(
                  "rounded px-3 text-sm font-medium whitespace-nowrap text-muted transition-colors hover:text-ink",
                  shared === s.value && "bg-beacon-soft text-ink",
                )}
              >
                {s.label}
              </button>
            ))}
          </div>
        </form>

        {query.isError && !query.data ? (
          <ErrorState
            title="Watches could not be loaded."
            error={query.error}
            onRetry={() => void query.refetch()}
          />
        ) : query.isPending ? (
          <ul aria-busy className="flex flex-col gap-2" aria-label="Loading watches">
            {Array.from({ length: 4 }, (_, i) => (
              <li
                key={i}
                data-testid="watch-skeleton"
                className="flex items-center gap-4 rounded-xl border border-line bg-surface px-4 py-3.5"
              >
                <span aria-hidden className="size-10 animate-pulse rounded-lg bg-line" />
                <span className="flex flex-1 flex-col gap-2">
                  <span aria-hidden className="h-3.5 w-40 animate-pulse rounded bg-line" />
                  <span aria-hidden className="h-3 w-64 animate-pulse rounded bg-line" />
                </span>
              </li>
            ))}
          </ul>
        ) : query.data.length === 0 ? (
          filtered ? (
            <div className="flex max-w-xl flex-col items-start gap-3 rounded-xl border border-dashed border-line px-6 py-8">
              <p className="font-display text-lg font-semibold">No watches match these filters</p>
              <Button variant="outline" onClick={clear}>
                Clear filters
              </Button>
            </div>
          ) : (
            <EmptyState title={item.emptyTitle} body={item.emptyBody} action={<NewWatchButton />} />
          )
        ) : (
          <div className="flex flex-col gap-3">
            <div className="flex min-h-9 items-center">
              {chosen.length === 0 ? (
                <span className="text-sm text-muted">
                  {query.data.length} watch{query.data.length === 1 ? "" : "es"}
                </span>
              ) : (
                <div
                  role="toolbar"
                  aria-label="Bulk actions"
                  className="flex flex-1 flex-wrap items-center gap-2 rounded-lg border border-beacon/40 bg-beacon-soft/40 px-3 py-1"
                >
                  <span className="mr-2 text-sm font-medium">{chosen.length} selected</span>
                  <DeleteSelected watches={chosen} onDone={setSelected} />
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() =>
                      setSelected(allSelected ? new Set() : new Set(owned.map((w) => w.id)))
                    }
                  >
                    {allSelected ? "Select none" : `Select all ${owned.length}`}
                  </Button>
                  <Button
                    variant="ghost"
                    size="sm"
                    className="ml-auto"
                    onClick={() => setSelected(new Set())}
                  >
                    Clear
                  </Button>
                </div>
              )}
            </div>
            <ul aria-label="Watches" className="flex flex-col gap-2">
              {query.data.map((w) => (
                <WatchRow
                  key={w.id}
                  watch={w}
                  now={now}
                  selected={selected.has(w.id)}
                  selecting={chosen.length > 0}
                  onToggle={() => toggle(w.id)}
                />
              ))}
            </ul>
          </div>
        )}
      </div>
    </SearchTerm>
  );
}

function WatchRow({
  watch,
  now,
  selected,
  selecting,
  onToggle,
}: {
  watch: Watch;
  now: Date;
  selected: boolean;
  /** Some row is selected: every owned tile shows as a checkbox. */
  selecting: boolean;
  onToggle: () => void;
}) {
  const owner = watch.permission === "owner";
  const on = owner && selected;
  return (
    <li
      className={cn(
        "flex items-center rounded-xl border border-line bg-surface transition-colors hover:border-beacon/50 hover:bg-beacon-soft/30",
        on && "border-beacon bg-beacon-soft/40",
      )}
    >
      {/* The kind tile is the selection toggle: a checkbox on hover or focus, and on every
          owned row once one is selected. */}
      <button
        type="button"
        aria-pressed={on}
        aria-label={`Select: ${watch.name}`}
        title={owner ? undefined : "Only the owner can delete it"}
        disabled={!owner}
        onClick={onToggle}
        className="group/tile relative ml-4 grid size-10 shrink-0 place-items-center rounded-lg disabled:cursor-not-allowed"
      >
        <KindIcon kind={watch.kind} />
        <span
          aria-hidden
          className={cn(
            "absolute inset-0 grid place-items-center rounded-lg border-2 opacity-0 transition-opacity",
            on ? "border-beacon bg-beacon text-surface" : "border-line bg-surface text-muted",
            owner && (on || selecting) && "opacity-100",
            owner && "group-hover/tile:opacity-100 group-focus-visible/tile:opacity-100",
          )}
        >
          <Check className={cn("size-5", !on && "opacity-40")} />
        </span>
      </button>
      <Link
        to={`/watches/${watch.id}`}
        className="flex min-w-0 flex-1 items-center gap-4 rounded-r-xl px-4 py-3.5"
      >
        <span className="flex min-w-0 flex-1 flex-col gap-0.5">
          <span className="flex flex-wrap items-center gap-2">
            <StatusDot watch={watch} />
            <span className="truncate font-medium">
              <Highlight text={watch.name} />
            </span>
            {watch.paused && <PausedBadge />}
            <SharedBadge watch={watch} />
          </span>
          <span className="truncate font-mono text-xs text-muted">
            <Highlight text={targetSummary(watch)} />
          </span>
        </span>
        <span
          className="hidden shrink-0 items-center gap-1.5 text-xs text-muted sm:flex"
          title={describeSchedule(watch.schedule) ?? watch.schedule}
        >
          <Clock aria-hidden className="size-3.5" />
          <span className="font-mono">{watch.schedule}</span>
        </span>
        {(!watch.paused || watch.running) && (
          <span className="w-28 shrink-0 text-right text-xs text-muted">
            {nextRunLabel(watch, now)}
          </span>
        )}
      </Link>
    </li>
  );
}

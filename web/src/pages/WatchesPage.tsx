import { keepPreviousData, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Clock, Play, Plus, Search, Trash2 } from "lucide-react";
import { createContext, use, useEffect, useId, useRef, useState, type Ref } from "react";
import { Link } from "react-router";

import { deleteWatch, listWatches, runAllWatches } from "@/api/client";
import { EmptyState } from "@/components/EmptyState";
import { ErrorState } from "@/components/ErrorState";
import { SelectionBar, SelectTile } from "@/components/Selection";
import { Button } from "@/components/ui/button";
import { Dialog, DialogActions } from "@/components/ui/dialog";
import { inputClass } from "@/components/ui/field";
import { DeleteWatchWarning, KindIcon, PausedBadge, StatusDot } from "@/components/WatchBits";
import { useToast } from "@/hooks/toast";
import { problemText } from "@/lib/problems";
import { describeSchedule, KINDS, targetSummary } from "@/lib/watchKinds";
import { nextRunLabel } from "@/lib/watchStatus";
import { cn } from "@/lib/utils";
import type { NavItem } from "@/nav";
import type { Watch, WatchKind } from "@/types/api";

// The search term, marked in the name and target a row shows. The server also matches the whole
// config, so a row can match with no mark.
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

export function NewWatchButton({ ref }: { ref?: Ref<HTMLAnchorElement> }) {
  return (
    <Button variant="primary" asChild>
      <Link ref={ref} to="/watches/new">
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
      title="Run every watch now; paused ones are skipped"
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
        dismissible={!del.isPending}
      >
        <DeleteWatchWarning />
        <ul className="max-h-48 list-inside list-disc overflow-y-auto text-sm">
          {watches.map((w) => (
            <li key={w.id}>{w.name}</li>
          ))}
        </ul>
        <DialogActions>
          <Button variant="ghost" disabled={del.isPending} onClick={() => setOpen(false)} autoFocus>
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
  const ids = { q: useId(), kind: useId() };
  const filters = { q: q.trim() || undefined, kind: kind || undefined };
  const filtered = Boolean(filters.q || filters.kind);
  const query = useQuery({
    queryKey: ["watches", filters],
    queryFn: () => listWatches(filters),
    placeholderData: keepPreviousData,
    // Keeps run statuses and "next in …" current while the list stays open.
    refetchInterval: 30_000,
  });
  const now = new Date();
  // Ids hidden by a filter stay selected but out of the action.
  const shown = query.data ?? [];
  const chosen = shown.filter((w) => selected.has(w.id));
  const newWatch = useRef<HTMLAnchorElement>(null);
  const selecting = chosen.length > 0;
  const wasSelecting = useRef(false);
  // The toolbar leaves with the focused button in it. Focus elsewhere, as in a filter field, stays put.
  useEffect(() => {
    if (wasSelecting.current && !selecting && document.activeElement === document.body) {
      newWatch.current?.focus();
    }
    wasSelecting.current = selecting;
  }, [selecting]);
  const toggle = (id: string) =>
    setSelected((s) => {
      const next = new Set(s);
      if (!next.delete(id)) next.add(id);
      return next;
    });
  const clear = () => {
    setQ("");
    setKind("");
  };

  return (
    <SearchTerm value={filters.q ?? ""}>
      <div className="mx-auto flex w-full max-w-6xl flex-col gap-6 px-5 py-8 md:px-10 md:py-12">
        <title>{`${item.label} – gedour`}</title>
        <div className="flex flex-wrap items-end justify-between gap-4">
          <div className="flex flex-col gap-2">
            <h1 className="font-display text-3xl font-semibold tracking-tight">{item.label}</h1>
            <p className="max-w-prose text-muted">Things gedour follows upstream for you.</p>
          </div>
          <div className="flex flex-wrap gap-2">
            <RunAllButton />
            <NewWatchButton ref={newWatch} />
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
            <SelectionBar
              count={`${shown.length} watch${shown.length === 1 ? "" : "es"}`}
              selected={chosen.length}
              total={shown.length}
              onSelect={(all) => setSelected(new Set(all ? shown.map((w) => w.id) : []))}
            >
              <DeleteSelected watches={chosen} onDone={setSelected} />
            </SelectionBar>
            <ul aria-label="Watches" className="flex flex-col gap-2">
              {query.data.map((w) => (
                <WatchRow
                  key={w.id}
                  watch={w}
                  now={now}
                  selected={selected.has(w.id)}
                  selecting={selecting}
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
  /** Some row is selected: every tile shows as a checkbox. */
  selecting: boolean;
  onToggle: () => void;
}) {
  return (
    <li
      className={cn(
        "flex items-center rounded-xl border border-line bg-surface transition-colors hover:border-beacon/50 hover:bg-beacon-soft/30",
        selected && "border-beacon bg-beacon-soft/40",
      )}
    >
      <SelectTile
        label={watch.name}
        selected={selected}
        selecting={selecting}
        onToggle={onToggle}
        className="ml-4"
      >
        <KindIcon kind={watch.kind} />
      </SelectTile>
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

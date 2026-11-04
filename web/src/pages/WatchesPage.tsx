import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { Clock, Plus, Search } from "lucide-react";
import { useId, useState } from "react";
import { Link } from "react-router";

import { listWatches } from "@/api/client";
import { EmptyState } from "@/components/EmptyState";
import { ErrorState } from "@/components/ErrorState";
import { Button } from "@/components/ui/button";
import { inputClass } from "@/components/ui/field";
import { KindIcon, PausedBadge, SharedBadge, StatusDot } from "@/components/WatchBits";
import { describeSchedule, KINDS, targetSummary } from "@/lib/watchKinds";
import { nextRunLabel } from "@/lib/watchStatus";
import { cn } from "@/lib/utils";
import type { NavItem } from "@/nav";
import type { SharedFilter, Watch, WatchKind } from "@/types/api";

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

export function WatchesPage({ item }: { item: NavItem }) {
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
  const clear = () => {
    setQ("");
    setKind("");
    setShared("all");
  };

  return (
    <div className="mx-auto flex w-full max-w-6xl flex-col gap-6 px-5 py-8 md:px-10 md:py-12">
      <title>{`${item.label} – gedour`}</title>
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div className="flex flex-col gap-2">
          <h1 className="font-display text-3xl font-semibold tracking-tight">{item.label}</h1>
          <p className="max-w-prose text-muted">
            Things gedour follows upstream for you, and those others share with you.
          </p>
        </div>
        <NewWatchButton />
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
        <ul aria-label="Watches" className="flex flex-col gap-2">
          {query.data.map((w) => (
            <WatchRow key={w.id} watch={w} now={now} />
          ))}
        </ul>
      )}
    </div>
  );
}

function WatchRow({ watch, now }: { watch: Watch; now: Date }) {
  return (
    <li>
      <Link
        to={`/watches/${watch.id}`}
        className="group flex items-center gap-4 rounded-xl border border-line bg-surface px-4 py-3.5 transition-colors hover:border-beacon/50 hover:bg-beacon-soft/30"
      >
        <KindIcon kind={watch.kind} />
        <span className="flex min-w-0 flex-1 flex-col gap-0.5">
          <span className="flex flex-wrap items-center gap-2">
            <StatusDot watch={watch} />
            <span className="truncate font-medium">{watch.name}</span>
            {watch.paused && <PausedBadge />}
            <SharedBadge watch={watch} />
          </span>
          <span className="truncate font-mono text-xs text-muted">{targetSummary(watch)}</span>
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

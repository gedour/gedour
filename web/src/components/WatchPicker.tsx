import { keepPreviousData, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { X } from "lucide-react";
import { useEffect, useId, useRef, useState, type ReactNode } from "react";
import { Link } from "react-router";

import { listWatches } from "@/api/client";
import { ErrorState } from "@/components/ErrorState";
import { Button } from "@/components/ui/button";
import {
  Highlight,
  KindIcon,
  PausedBadge,
  SearchTerm,
  WatchSearchFields,
} from "@/components/WatchBits";
import { useDebounced } from "@/hooks/useDebounced";
import { problemText } from "@/lib/problems";
import { pickLabel, type WatchPick } from "@/lib/watchPicks";
import { targetSummary } from "@/lib/watchKinds";
import type { Watch, WatchFilters, WatchKind } from "@/types/api";

/** The most watches a rule lists: the listWatches page maximum, so one call covers "Select all matching". */
export const MAX_PICKS = 500;
const SHOWN = 100;
const CHIPS = 10;
const pickOf = (w: Watch): WatchPick => ({ id: w.id, name: w.name, watch: w });

/** 014: a rule's scope, all watches or the picked ones, searched server side. */
export function WatchPicker({
  all,
  setAll,
  picks,
  setPicks,
  errors,
}: {
  all: boolean;
  setAll: (v: boolean) => void;
  picks: WatchPick[];
  setPicks: (v: WatchPick[]) => void;
  /** One line each, shown on the field. */
  errors?: string[];
}) {
  const id = useId();
  const error = errors?.length ? errors : undefined;
  const errorId = `${id}-error`;
  const [q, setQ] = useState("");
  const [kind, setKind] = useState<WatchKind | "">("");
  const [selectedOnly, setSelectedOnly] = useState(false);
  const [unfolded, setUnfolded] = useState(false);
  const [notice, setNotice] = useState("");
  const term = useDebounced(q.trim(), 300);
  const filters = { q: term || undefined, kind: kind || undefined };
  const filtered = Boolean(filters.q || filters.kind);
  const list = useQuery({
    // One more than shown tells whether there are more.
    queryKey: ["watches", { ...filters, limit: SHOWN + 1 }],
    queryFn: () => listWatches({ ...filters, limit: SHOWN + 1 }),
    placeholderData: keepPreviousData,
    enabled: !all,
  });
  const picked = new Set(picks.map((p) => p.id));
  // Details of the picks the rule came with, once a list showed their rows (014-AC17).
  const client = useQueryClient();
  const seen = new Map(
    client
      .getQueriesData<Watch[]>({ queryKey: ["watches"] })
      .flatMap(([, d]) => d ?? [])
      .map((w) => [w.id, w]),
  );
  const box = useRef<HTMLDivElement>(null);
  const toggleRef = useRef<HTMLInputElement>(null);
  // What to focus once a removed chip or "Selected only" row is gone: its list and index.
  const refocus = useRef<{ list: "chips" | "rows"; at: number } | null>(null);
  useEffect(() => {
    const r = refocus.current;
    if (!r) return;
    refocus.current = null;
    const items = box.current?.querySelectorAll<HTMLElement>(
      r.list === "chips" ? "[data-chip-remove]" : "[data-pick-row]",
    );
    const next = items?.[Math.min(r.at, items.length - 1)];
    (
      next ??
      (r.list === "chips"
        ? box.current?.querySelector<HTMLElement>("input[type=search]")
        : toggleRef.current)
    )?.focus();
  }, [picks]);
  const bulk = useMutation({
    // The typed search, not the debounced one: a click right after typing acts on what is shown in the box.
    mutationFn: async ({ op, filters }: { op: "select" | "clear"; filters: WatchFilters }) => ({
      op,
      matches: await listWatches({ ...filters, limit: MAX_PICKS }),
    }),
    onMutate: () => setNotice(""),
    onSuccess: ({ op, matches }) => {
      const full = matches.length >= MAX_PICKS;
      if (op === "clear") {
        const ids = new Set(matches.map((w) => w.id));
        setPicks(picks.filter((p) => !ids.has(p.id)));
        if (full)
          setNotice(
            `${MAX_PICKS} or more watches match: only the first ${MAX_PICKS} were cleared. Refine the search.`,
          );
        return;
      }
      if (full) {
        setNotice(`${MAX_PICKS} or more watches match: refine the search. Nothing was picked.`);
        return;
      }
      const add = matches.filter((w) => !picked.has(w.id));
      if (picks.length + add.length > MAX_PICKS) {
        setNotice(
          `That would pick ${picks.length + add.length} watches, and a rule takes ${MAX_PICKS} at most. Nothing was picked.`,
        );
        return;
      }
      setPicks([...picks, ...add.map(pickOf)]);
    },
    onError: (err) => setNotice(`Could not load the matching watches: ${problemText(err)}`),
  });
  const search = (set: () => void) => {
    set();
    setNotice("");
  };
  const toggle = (p: WatchPick, on: boolean) =>
    setPicks(on ? [...picks, p] : picks.filter((x) => x.id !== p.id));
  const typed = (op: "select" | "clear") =>
    bulk.mutate({ op, filters: { q: q.trim() || undefined, kind: kind || undefined } });
  // 014-AC18: an unmounted control drops focus to the page; it goes to the next one instead.
  const remove = (p: WatchPick, list: "chips" | "rows") => {
    // Chips are the first picks in order, so a pick's index is its chip's too.
    refocus.current = { list, at: picks.findIndex((x) => x.id === p.id) };
    toggle(p, false);
  };
  const chips = unfolded ? picks : picks.slice(0, CHIPS);

  return (
    <fieldset className="flex min-w-0 flex-col gap-2">
      <legend className="mb-1.5 text-sm font-medium">Watches</legend>
      {[
        { on: true, text: "All watches", hint: "Every watch you have, new ones too." },
        { on: false, text: "Selected watches", hint: `Up to ${MAX_PICKS} you pick.` },
      ].map((o) => (
        <label key={o.text} className="flex items-start gap-2 text-sm">
          <input
            type="radio"
            name={`${id}-scope`}
            checked={all === o.on}
            aria-describedby={error ? errorId : undefined}
            onChange={() => setAll(o.on)}
            className="mt-0.5 accent-beacon"
          />
          <span>
            {o.text} <span className="text-xs text-muted">{o.hint}</span>
          </span>
        </label>
      ))}
      {error && (
        <div id={errorId} className="flex flex-col gap-1 text-sm text-rose-700 dark:text-rose-300">
          {error.map((m) => (
            <p key={m}>{m}</p>
          ))}
        </div>
      )}
      {!all && (
        <div ref={box} className="flex min-w-0 flex-col gap-3 rounded-lg border border-line p-3">
          {picks.length > 0 && (
            <ul aria-label="Picked watches" className="flex flex-wrap gap-1.5">
              {chips.map((p) => (
                <li
                  key={p.id}
                  className="inline-flex max-w-64 items-center gap-1 rounded-md bg-bg py-0.5 pr-0.5 pl-2 text-xs font-medium ring-1 ring-line ring-inset"
                >
                  <span className={p.name === null ? "truncate text-muted italic" : "truncate"}>
                    {pickLabel(p)}
                  </span>
                  <button
                    type="button"
                    aria-label={`Remove ${pickLabel(p)}`}
                    data-chip-remove
                    onClick={() => remove(p, "chips")}
                    className="grid size-5 shrink-0 place-items-center rounded text-muted hover:bg-line/60 hover:text-ink"
                  >
                    <X aria-hidden className="size-3" />
                  </button>
                </li>
              ))}
              {picks.length > CHIPS && (
                <li>
                  <button
                    type="button"
                    aria-expanded={unfolded}
                    onClick={() => setUnfolded(!unfolded)}
                    className="rounded-md px-2 py-0.5 text-xs font-medium underline-offset-4 hover:underline"
                  >
                    {unfolded ? "Show fewer" : `+${picks.length - CHIPS} more`}
                  </button>
                </li>
              )}
            </ul>
          )}
          <div
            role="search"
            aria-label="Filter watches to pick"
            className="flex flex-wrap items-end gap-3"
          >
            <WatchSearchFields
              q={q}
              setQ={(v) => search(() => setQ(v))}
              kind={kind}
              setKind={(v) => search(() => setKind(v))}
              disabled={selectedOnly}
            />
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <Button
              type="button"
              variant="outline"
              size="sm"
              disabled={selectedOnly || bulk.isPending}
              onClick={() => typed("select")}
            >
              Select all matching
            </Button>
            <Button
              type="button"
              variant="outline"
              size="sm"
              disabled={selectedOnly || bulk.isPending || picks.length === 0}
              onClick={() => typed("clear")}
            >
              Clear matching
            </Button>
            <label className="inline-flex items-center gap-2 text-sm">
              <input
                ref={toggleRef}
                type="checkbox"
                checked={selectedOnly}
                onChange={(e) => setSelectedOnly(e.target.checked)}
                className="size-4 accent-beacon"
              />
              Selected only
            </label>
            <p className="ml-auto text-sm text-muted" aria-live="polite">
              {picks.length} selected
            </p>
          </div>
          {/* Always there: a live region added with its text is not announced everywhere. */}
          <p role="status" className="text-sm text-amber-800 empty:-mt-3 dark:text-amber-300">
            {notice}
          </p>
          {!selectedOnly && list.isError && !list.data ? (
            <ErrorState
              title="Watches could not be loaded."
              error={list.error}
              onRetry={() => void list.refetch()}
            />
          ) : !selectedOnly && list.isPending ? (
            <p className="text-sm text-muted" aria-busy>
              Loading watches…
            </p>
          ) : (
            <SearchTerm value={selectedOnly ? "" : term}>
              <PickList
                rows={
                  selectedOnly
                    ? picks.map((p) => ({ ...p, watch: p.watch ?? seen.get(p.id) }))
                    : (list.data ?? []).slice(0, SHOWN).map(pickOf)
                }
                picked={picked}
                full={picks.length >= MAX_PICKS ? `${id}-full` : undefined}
                toggle={(p, on) => (selectedOnly && !on ? remove(p, "rows") : toggle(p, on))}
                empty={
                  selectedOnly ? (
                    "No watch selected yet."
                  ) : filtered ? (
                    "No watch matches the search."
                  ) : (
                    <>
                      You have no watch yet.{" "}
                      <Link to="/watches/new" className="font-medium underline underline-offset-4">
                        Create one
                      </Link>
                      .
                    </>
                  )
                }
              />
            </SearchTerm>
          )}
          {picks.length >= MAX_PICKS && (
            <p id={`${id}-full`} className="text-xs text-muted">
              {MAX_PICKS} watches picked, the most a rule takes: unpick one to pick another.
            </p>
          )}
          {!selectedOnly && (list.data?.length ?? 0) > SHOWN && (
            <p className="text-xs text-muted">
              Showing the first {SHOWN} matches: refine the search
            </p>
          )}
        </div>
      )}
    </fieldset>
  );
}

function PickList({
  rows,
  picked,
  full,
  toggle,
  empty,
}: {
  rows: WatchPick[];
  picked: Set<string>;
  /** Set once the picks reached the cap: the id of the note saying so, for the rows it disables. */
  full?: string;
  toggle: (p: WatchPick, on: boolean) => void;
  empty: ReactNode;
}) {
  if (rows.length === 0) return <p className="text-sm text-muted">{empty}</p>;
  return (
    <ul
      aria-label="Watches to pick"
      className="flex max-h-80 flex-col gap-0.5 overflow-y-auto rounded-md border border-line p-1"
    >
      {rows.map((p) => (
        <li key={p.id}>
          <label className="flex items-center gap-3 rounded-md px-2 py-1.5 text-sm hover:bg-beacon-soft/30 has-checked:bg-beacon-soft/40">
            <input
              type="checkbox"
              data-pick-row
              checked={picked.has(p.id)}
              disabled={Boolean(full) && !picked.has(p.id)}
              aria-describedby={full && !picked.has(p.id) ? full : undefined}
              onChange={(e) => toggle(p, e.target.checked)}
              className="size-4 shrink-0 accent-beacon"
            />
            {p.watch && <KindIcon kind={p.watch.kind} className="size-8" />}
            <span className="flex min-w-0 flex-1 flex-col">
              <span className={p.name === null ? "truncate text-muted italic" : "truncate"}>
                <Highlight text={pickLabel(p)} />
              </span>
              {/* Spaces in the checkbox's name; a flex box renders none. */}{" "}
              {p.watch && (
                <span className="truncate font-mono text-xs text-muted">
                  <Highlight text={targetSummary(p.watch)} />
                </span>
              )}
            </span>{" "}
            {p.watch?.paused && <PausedBadge />}
          </label>
        </li>
      ))}
    </ul>
  );
}

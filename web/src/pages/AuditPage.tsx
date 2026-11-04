import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { ChevronRight, RotateCw } from "lucide-react";
import { Fragment, useId, useState } from "react";

import { listAudit, listAuditActors } from "@/api/client";
import { EmptyState } from "@/components/EmptyState";
import { Button } from "@/components/ui/button";
import { Tooltip } from "@/components/ui/tooltip";
import { actionTone, TARGET_TYPES } from "@/lib/audit";
import { diffJSON, formatValue, type ChangeKind } from "@/lib/diff";
import { dayStartISO, formatAbsolute, formatRelative } from "@/lib/time";
import { cn } from "@/lib/utils";
import type { NavItem } from "@/nav";
import type { AuditFilters, AuditItem } from "@/types/api";

const PAGE_SIZE = 50;
// Literal class names: Tailwind only generates what it can read in the source.
const SKELETON_WIDTHS = ["w-24", "w-32", "w-20", "w-36", "w-6"];

const fieldClass =
  "h-9 rounded-md border border-line bg-surface px-2.5 text-sm text-ink shadow-xs focus-visible:border-beacon";

export function AuditPage({ item }: { item: NavItem }) {
  const [actor, setActor] = useState("");
  const [targetType, setTargetType] = useState("");
  const [from, setFrom] = useState("");
  const [to, setTo] = useState("");
  const ids = { actor: useId(), type: useId(), from: useId(), to: useId() };
  const actors = useQuery({ queryKey: ["audit-actors"], queryFn: listAuditActors });

  const filters: AuditFilters = {
    actor: actor || undefined,
    target_type: targetType || undefined,
    from: from ? dayStartISO(from) : undefined,
    to: to ? dayStartISO(to, true) : undefined,
  };
  const filtered = Boolean(actor || targetType || from || to);
  const query = useInfiniteQuery({
    queryKey: ["audit", filters],
    queryFn: ({ pageParam }) => listAudit(filters, pageParam, PAGE_SIZE),
    initialPageParam: "",
    getNextPageParam: (last) => last.next_cursor || undefined,
  });
  const items = query.data?.pages.flatMap((p) => p.items) ?? [];

  const clear = () => {
    setActor("");
    setTargetType("");
    setFrom("");
    setTo("");
  };

  return (
    <div className="mx-auto flex w-full max-w-6xl flex-col gap-6 px-5 py-8 md:px-10 md:py-12">
      <title>{`${item.label} – gedour`}</title>
      <div className="flex flex-col gap-2">
        <h1 className="font-display text-3xl font-semibold tracking-tight">{item.label}</h1>
        <p className="max-w-prose text-muted">
          Every change you made, and every change on watches you can see, newest first. Secrets are
          never shown.
        </p>
      </div>

      <form
        aria-label="Filter audit entries"
        className="flex flex-wrap items-end gap-3"
        onSubmit={(e) => e.preventDefault()}
      >
        <div className="flex flex-col gap-1">
          <label htmlFor={ids.actor} className="text-xs font-medium text-muted">
            Actor
          </label>
          <select
            id={ids.actor}
            value={actor}
            onChange={(e) => setActor(e.target.value)}
            className={cn(fieldClass, "pr-8")}
          >
            <option value="">Everyone</option>
            {actors.data?.map((a) => (
              <option key={a.id} value={a.id}>
                {a.name || "Unnamed user"}
              </option>
            ))}
          </select>
        </div>
        <div className="flex flex-col gap-1">
          <label htmlFor={ids.type} className="text-xs font-medium text-muted">
            Target type
          </label>
          <select
            id={ids.type}
            value={targetType}
            onChange={(e) => setTargetType(e.target.value)}
            className={cn(fieldClass, "pr-8")}
          >
            <option value="">All types</option>
            {TARGET_TYPES.map((t) => (
              <option key={t.value} value={t.value}>
                {t.label}
              </option>
            ))}
          </select>
        </div>
        <div className="flex flex-col gap-1">
          <label htmlFor={ids.from} className="text-xs font-medium text-muted">
            From
          </label>
          <input
            id={ids.from}
            type="date"
            value={from}
            max={to || undefined}
            onChange={(e) => setFrom(e.target.value)}
            className={fieldClass}
          />
        </div>
        <div className="flex flex-col gap-1">
          <label htmlFor={ids.to} className="text-xs font-medium text-muted">
            To
          </label>
          <input
            id={ids.to}
            type="date"
            value={to}
            min={from || undefined}
            onChange={(e) => setTo(e.target.value)}
            className={fieldClass}
          />
        </div>
        {filtered && (
          <Button type="button" variant="ghost" onClick={clear}>
            Clear filters
          </Button>
        )}
      </form>

      {query.isError && !query.data ? (
        <div
          role="alert"
          className="flex max-w-xl flex-col items-start gap-3 rounded-xl border border-rose-500/40 bg-rose-500/8 px-6 py-5"
        >
          <p className="font-medium">The audit log could not be loaded.</p>
          <p className="text-sm text-muted">{query.error.message}</p>
          <Button variant="outline" onClick={() => void query.refetch()}>
            <RotateCw aria-hidden />
            Retry
          </Button>
        </div>
      ) : !query.isPending && items.length === 0 ? (
        filtered ? (
          <div className="flex max-w-xl flex-col items-start gap-3 rounded-xl border border-dashed border-line px-6 py-8">
            <p className="font-display text-lg font-semibold">No entries match these filters</p>
            <Button variant="outline" onClick={clear}>
              Clear filters
            </Button>
          </div>
        ) : (
          <EmptyState title={item.emptyTitle} body={item.emptyBody} />
        )
      ) : (
        <AuditTable items={items} loading={query.isPending} />
      )}

      {query.hasNextPage && (
        <Button
          variant="outline"
          className="self-center"
          disabled={query.isFetchingNextPage}
          onClick={() => void query.fetchNextPage()}
        >
          {query.isFetchingNextPage ? "Loading…" : "Load more"}
        </Button>
      )}
      {query.isFetchNextPageError && (
        <p role="alert" className="self-center text-sm text-rose-700 dark:text-rose-300">
          The next page could not be loaded.
        </p>
      )}
    </div>
  );
}

function AuditTable({ items, loading }: { items: AuditItem[]; loading: boolean }) {
  const [open, setOpen] = useState<Set<number>>(new Set());
  const toggle = (id: number) =>
    setOpen((prev) => {
      const next = new Set(prev);
      if (!next.delete(id)) next.add(id);
      return next;
    });
  const now = new Date();

  return (
    <div className="overflow-x-auto rounded-xl border border-line bg-surface">
      <table className="w-full min-w-[640px] text-left text-sm" aria-busy={loading}>
        <caption className="sr-only">Audit entries, newest first</caption>
        <thead className="border-b border-line text-xs tracking-wide text-muted uppercase">
          <tr>
            <th scope="col" className="px-4 py-3 font-medium">
              Time
            </th>
            <th scope="col" className="px-4 py-3 font-medium">
              Actor
            </th>
            <th scope="col" className="px-4 py-3 font-medium">
              Action
            </th>
            <th scope="col" className="px-4 py-3 font-medium">
              Target
            </th>
            <th scope="col" className="w-12 px-2 py-3">
              <span className="sr-only">Details</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {loading
            ? Array.from({ length: 5 }, (_, i) => (
                <tr
                  key={i}
                  data-testid="audit-skeleton"
                  className="border-b border-line last:border-0"
                >
                  {SKELETON_WIDTHS.map((w) => (
                    <td key={w} className="px-4 py-3.5">
                      <span
                        aria-hidden
                        className={cn("block h-3.5 animate-pulse rounded bg-line", w)}
                      />
                    </td>
                  ))}
                </tr>
              ))
            : items.map((it) => (
                <AuditRow
                  key={it.id}
                  item={it}
                  now={now}
                  open={open.has(it.id)}
                  onToggle={() => toggle(it.id)}
                />
              ))}
        </tbody>
      </table>
      {loading && <span className="sr-only">Loading audit entries</span>}
    </div>
  );
}

function AuditRow({
  item,
  now,
  open,
  onToggle,
}: {
  item: AuditItem;
  now: Date;
  open: boolean;
  onToggle: () => void;
}) {
  const detailsId = useId();
  const at = new Date(item.at);
  const absolute = formatAbsolute(at);
  return (
    <Fragment>
      <tr className={cn("border-b border-line last:border-0", open && "bg-bg/60")}>
        <td className="px-4 py-3 whitespace-nowrap">
          <Tooltip label={absolute} side="bottom">
            <time
              dateTime={item.at}
              tabIndex={0}
              aria-label={absolute}
              className="cursor-default rounded text-ink tabular-nums"
            >
              {formatRelative(at, now)}
            </time>
          </Tooltip>
        </td>
        <td className="max-w-48 truncate px-4 py-3">{item.actor_name || "System"}</td>
        <td className="px-4 py-3">
          <span
            className={cn(
              "inline-flex rounded-md px-2 py-0.5 font-mono text-xs font-medium ring-1 ring-inset",
              actionTone(item.action),
            )}
          >
            {item.action}
          </span>
        </td>
        <td className="px-4 py-3">
          <span className="text-muted">{item.target_type}</span>
          {item.target_id && (
            <code className="ml-2 rounded bg-bg px-1.5 py-0.5 font-mono text-xs">
              {item.target_id.length > 13 ? `${item.target_id.slice(0, 8)}…` : item.target_id}
            </code>
          )}
        </td>
        <td className="px-2 py-2 text-right">
          <Button
            variant="ghost"
            size="icon"
            aria-expanded={open}
            aria-controls={detailsId}
            aria-label={`${open ? "Hide" : "Show"} details of ${item.action}`}
            onClick={onToggle}
          >
            <ChevronRight
              aria-hidden
              className={cn(
                "transition-transform motion-reduce:transition-none",
                open && "rotate-90",
              )}
            />
          </Button>
        </td>
      </tr>
      {open && (
        <tr id={detailsId} className="border-b border-line bg-bg/60 last:border-0">
          <td colSpan={5} className="px-4 pt-1 pb-4">
            <AuditDetails item={item} absolute={absolute} />
          </td>
        </tr>
      )}
    </Fragment>
  );
}

const OLD = "text-rose-800 line-through decoration-rose-500/60 dark:text-rose-300";
const NEW = "text-emerald-800 dark:text-emerald-300";
const dash = <span className="text-muted">—</span>;

function cellStyle(kind: ChangeKind, side: "before" | "after"): string {
  if (kind === "same") return "text-muted";
  return side === "before" ? OLD : NEW;
}

function AuditDetails({ item, absolute }: { item: AuditItem; absolute: string }) {
  const changes = diffJSON(item.before, item.after);
  return (
    <div className="flex flex-col gap-3">
      <dl className="flex flex-wrap gap-x-6 gap-y-1 text-xs text-muted">
        <div className="flex gap-1.5">
          <dt>At</dt>
          <dd className="text-ink">{absolute}</dd>
        </div>
        {item.target_id && (
          <div className="flex gap-1.5">
            <dt>Target ID</dt>
            <dd className="font-mono text-ink">{item.target_id}</dd>
          </div>
        )}
        {item.request_id && (
          <div className="flex gap-1.5">
            <dt>Request</dt>
            <dd className="font-mono text-ink">{item.request_id}</dd>
          </div>
        )}
      </dl>
      {changes.length === 0 ? (
        <p className="text-sm text-muted">No state recorded for this entry.</p>
      ) : (
        <table className="w-full rounded-lg border border-line bg-surface text-left text-xs">
          <caption className="sr-only">Changes recorded by {item.action}</caption>
          <thead className="text-muted">
            <tr className="border-b border-line">
              <th scope="col" className="px-3 py-2 font-medium">
                Field
              </th>
              <th scope="col" className="px-3 py-2 font-medium">
                Before
              </th>
              <th scope="col" className="px-3 py-2 font-medium">
                After
              </th>
            </tr>
          </thead>
          <tbody className="font-mono">
            {changes.map((c) => (
              <tr key={c.path} data-kind={c.kind} className="border-b border-line last:border-0">
                <th scope="row" className="px-3 py-1.5 font-medium text-ink">
                  {c.path}
                </th>
                <td className="px-3 py-1.5 break-all">
                  {c.before === undefined ? (
                    dash
                  ) : (
                    <span className={cellStyle(c.kind, "before")}>{formatValue(c.before)}</span>
                  )}
                </td>
                <td className="px-3 py-1.5 break-all">
                  {c.after === undefined ? (
                    dash
                  ) : (
                    <span className={cellStyle(c.kind, "after")}>{formatValue(c.after)}</span>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

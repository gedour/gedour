import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import {
  Archive,
  ArchiveRestore,
  ExternalLink,
  Flag,
  Mail,
  MailOpen,
  Telescope,
  X,
  type LucideIcon,
} from "lucide-react";
import { useEffect, useEffectEvent, useId, useRef, useState } from "react";
import { Link, useLocation, useSearchParams } from "react-router";

import { inboxAction, listInbox } from "@/api/client";
import { EmptyState } from "@/components/EmptyState";
import { ErrorState } from "@/components/ErrorState";
import { SelectionBar, SelectTile } from "@/components/Selection";
import { PriorityBadge } from "@/components/PriorityBadge";
import { Timestamp } from "@/components/Timestamp";
import { Button } from "@/components/ui/button";
import { Dialog, DialogActions } from "@/components/ui/dialog";
import { KindIcon } from "@/components/WatchBits";
import { useToast } from "@/hooks/toast";
import { useMediaQuery } from "@/hooks/useMediaQuery";
import { COUNT_KEY, LIST_KEY, useInboxCount } from "@/hooks/useInbox";
import { summarizeEvent, TONE_CLASS } from "@/lib/events";
import { groupByDay } from "@/lib/inbox";
import { problemText } from "@/lib/problems";
import { formatAbsolute, formatRelative } from "@/lib/time";
import { cn } from "@/lib/utils";
import type { NavItem } from "@/nav";
import type { InboxAction, InboxItem, InboxState } from "@/types/api";

const TABS: { state: InboxState; label: string }[] = [
  { state: "unread", label: "Unread" },
  { state: "all", label: "All" },
  { state: "archived", label: "Archived" },
];

const EMPTY: Record<InboxState, { title: string; body: string } | null> = {
  unread: {
    title: "You are all caught up",
    body: "New changes land here as your watches see them.",
  },
  all: null,
  archived: { title: "Nothing archived", body: "Archive items (e) to keep this list short." },
};

// Shortcuts must not fire while typing in a field (a checkbox or radio takes no text), nor inside a
// menu, an open dialog or the navigation drawer, which own their keys.
function ignoresShortcuts(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false;
  const typing =
    (target instanceof HTMLInputElement && !["checkbox", "radio"].includes(target.type)) ||
    ["TEXTAREA", "SELECT"].includes(target.tagName) ||
    target.isContentEditable;
  return typing || target.closest("[role=menu], dialog[open], #app-sidebar") !== null;
}

const rowId = (id: number) => `inbox-item-${id}`;

const now = () => new Date().toISOString();
const PATCH: Record<InboxAction, () => Partial<InboxItem>> = {
  read: () => ({ read_at: now() }),
  unread: () => ({ read_at: null, archived_at: null }),
  archive: () => ({ archived_at: now() }),
  unarchive: () => ({ archived_at: null }),
};

const inView = (it: InboxItem, state: InboxState) =>
  state === "archived"
    ? Boolean(it.archived_at)
    : !it.archived_at && (state === "all" || !it.read_at);

/**
 * The listed items with pending actions applied, minus those that left the tab; the opened item stays
 * in place (newest first, by id like the server) as long as its panel shows.
 */
function visibleItems(
  fetched: InboxItem[],
  patches: Map<number, Partial<InboxItem>>,
  state: InboxState,
  opened: InboxItem | null,
): InboxItem[] {
  const out = fetched
    .map((it) => (patches.has(it.id) ? { ...it, ...patches.get(it.id) } : it))
    .filter((it) => it.id === opened?.id || inView(it, state));
  if (opened && !out.some((it) => it.id === opened.id)) {
    // Opened from a loaded page, so older than every loaded item means last.
    const at = out.findIndex((it) => it.id < opened.id);
    out.splice(at < 0 ? out.length : at, 0, opened);
  }
  return out;
}

// The side panel needs the lg layout's room; below it the detail is a dialog.
const WIDE_QUERY = "(min-width: 1024px)";

export function InboxPage({ item: nav }: { item: NavItem }) {
  const [params, setParams] = useSearchParams();
  const raw = params.get("state");
  const state: InboxState = raw === "unread" || raw === "archived" ? raw : "all";
  const highOnly = params.get("priority") === "high";
  const query = useInfiniteQuery({
    queryKey: [...LIST_KEY, state, highOnly],
    queryFn: ({ pageParam }) => listInbox(state, pageParam, highOnly),
    initialPageParam: "",
    getNextPageParam: (last) => last.next_cursor,
  });
  const unread = useInboxCount().data;
  const client = useQueryClient();
  const toast = useToast();
  const [selected, setSelected] = useState<Set<number>>(new Set());
  const [focus, setFocus] = useState(0);
  // The j/k cursor shows, and shortcuts act, only once a row was focused or j/k pressed: never on a row
  // the reader did not pick.
  const [cursor, setCursor] = useState(false);
  // The opened item itself, not its id: in the Unread tab, reading it drops it from the list, and the
  // panel must stay.
  const [opened, setOpened] = useState<InboxItem | null>(null);
  // Actions shown before the server confirms them: a row leaves its tab at once, so the next one comes
  // under the pointer and a quick action can be repeated in place.
  const [patches, setPatches] = useState<Map<number, Partial<InboxItem>>>(new Map());
  const items = visibleItems(
    query.data?.pages.flatMap((p) => p.items) ?? [],
    patches,
    state,
    opened,
  );
  const wide = useMediaQuery(WIDE_QUERY);
  const closeRef = useRef<HTMLButtonElement>(null);
  const dialogOpen = Boolean(opened) && !wide;
  // After the dialog's own effect opened it: a closed dialog's content cannot take focus.
  useEffect(() => {
    if (dialogOpen) closeRef.current?.focus();
  }, [dialogOpen]);

  // Items new since the last render of this tab play the arrival animation.
  const ids = items.map((i) => i.id).join(",");
  const view = `${state}/${highOnly}`;
  const [seen, setSeen] = useState<{ view: string; ids: string } | null>(null);
  const [arrived, setArrived] = useState<Set<number>>(new Set());
  if (query.isSuccess && (seen?.view !== view || seen.ids !== ids)) {
    const before = seen?.view === view ? new Set(seen.ids.split(",").map(Number)) : null;
    setArrived(
      before ? new Set(items.filter((i) => !before.has(i.id)).map((i) => i.id)) : new Set(),
    );
    setSeen({ view, ids });
    // Selection and focus follow the list: gone items drop out.
    setSelected((s) => new Set([...s].filter((id) => items.some((i) => i.id === id))));
    setFocus((f) => Math.min(f, Math.max(items.length - 1, 0)));
  }

  // Counted here, not with isMutating(): actions settling in one tick would each still see the others.
  const inFlight = useRef(0);
  const act = useMutation({
    mutationFn: ({ action, ids }: { action: InboxAction; ids: number[]; patch?: object }) =>
      inboxAction(action, ids),
    // Only this action's patches: a later action on the same row keeps its own.
    onError: (err, { patch }) => {
      setPatches((m) => new Map([...m].filter(([, p]) => p !== patch)));
      toast(`Could not update the inbox: ${problemText(err)}`, "error");
    },
    // Patches go once the list reflects every action sent: a refetch landing between two actions
    // would bring back a row the later one already hid.
    onSettled: async () => {
      void client.invalidateQueries({ queryKey: COUNT_KEY });
      if (--inFlight.current > 0) return;
      await client.invalidateQueries({ queryKey: LIST_KEY });
      if (inFlight.current === 0) setPatches(new Map());
    },
  });
  const run = (action: InboxAction, ids: number[]) => {
    if (ids.length === 0) return;
    const patch = PATCH[action]();
    setPatches((m) => new Map([...m, ...ids.map((id) => [id, patch] as const)]));
    setOpened((o) => (o && ids.includes(o.id) ? { ...o, ...patch } : o));
    inFlight.current++;
    act.mutate({ action, ids, patch });
  };
  const actOnOpened = (a: InboxAction) => {
    if (opened) run(a, [opened.id]);
  };
  const open = (it: InboxItem) => {
    setOpened(it);
    if (!it.read_at) run("read", [it.id]);
  };
  // A notification's link (/inbox?event=<id>, 010-AC13) opens that event's item once the list holds it.
  // Keyed by location: the same link followed again opens it again.
  // ponytail: only items of the loaded pages are found; an item API would reach older ones.
  const linked = Number(params.get("event")) || 0;
  const { key } = useLocation();
  const [link, setLink] = useState<{ key: string; event: number } | null>(null);
  // After the mount refetch: a cached list from an earlier visit lacks the event just pushed. A failed
  // refetch resolves too, against what is cached, so the link never hangs in the URL.
  if (linked && link?.key !== key && (query.isSuccess || query.isError) && !query.isFetching) {
    setLink({ key, event: linked });
    const hit = items.find((i) => i.event.id === linked);
    if (hit) setOpened(hit.read_at ? hit : { ...hit, ...PATCH.read() });
  }
  const dropLink = useEffectEvent(() => {
    const hit = items.find((i) => i.event.id === link?.event);
    // Not run(): its patch is state, set above while rendering.
    if (hit && !hit.read_at) {
      inFlight.current++;
      act.mutate({ action: "read", ids: [hit.id] });
    }
    if (!hit)
      toast(
        "That event is not among the inbox items loaded here: it may be archived or older.",
        "warning",
      );
    setParams(
      (p) => {
        p.delete("event");
        return p;
      },
      { replace: true },
    );
  });
  useEffect(() => {
    if (link) dropLink();
  }, [link]);
  const close = () => {
    const id = opened?.id;
    setOpened(null);
    if (id !== undefined) document.getElementById(rowId(id))?.focus();
  };
  const toggle = (id: number) =>
    setSelected((s) => {
      const next = new Set(s);
      if (!next.delete(id)) next.add(id);
      return next;
    });
  const moveTo = (i: number) => {
    const it = items[i];
    if (!it) return;
    setFocus(i);
    setCursor(true);
    document.getElementById(rowId(it.id))?.focus();
  };

  const onKey = useEffectEvent((e: KeyboardEvent) => {
    if (e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey || ignoresShortcuts(e.target)) {
      return;
    }
    const it = cursor ? items[focus] : undefined;
    const step = cursor ? 1 : 0;
    switch (e.key) {
      case "j":
        moveTo(Math.min(focus + step, items.length - 1));
        break;
      case "k":
        moveTo(Math.max(focus - step, 0));
        break;
      case "e":
        if (it) run(it.archived_at ? "unarchive" : "archive", [it.id]);
        break;
      case "u":
        if (it) run(it.read_at ? "unread" : "read", [it.id]);
        break;
      case "x":
        if (it) toggle(it.id);
        break;
      case "Enter":
        if (it && e.target === document.body) open(it);
        return;
      case "Escape":
        if (opened) close();
        return;
      default:
        return;
    }
    e.preventDefault();
  });
  useEffect(() => {
    const handler = (e: KeyboardEvent) => onKey(e);
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, []);

  const show = (next: InboxState, nextHighOnly: boolean) => {
    setParams({
      ...(next !== "all" && { state: next }),
      ...(nextHighOnly && { priority: "high" }),
    });
    setSelected(new Set());
    setOpened(null);
    setFocus(0);
    setCursor(false);
  };

  const chosen = [...selected];
  const empty = highOnly
    ? { title: "Nothing high priority here", body: "High priority rules bring their events here." }
    : (EMPTY[state] ?? { title: nav.emptyTitle, body: nav.emptyBody });
  const tabsId = useId();

  return (
    <div className="mx-auto flex w-full max-w-6xl flex-col gap-6 px-5 py-8 md:px-10 md:py-12">
      <title>{unread ? `(${unread}) ${nav.label} – gedour` : `${nav.label} – gedour`}</title>
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div className="flex flex-col gap-2">
          <h1 className="font-display text-3xl font-semibold tracking-tight">{nav.label}</h1>
          <p className="max-w-prose text-muted">
            Changes your rules let through, newest first.{" "}
            <span className="hidden text-xs md:inline">
              <kbd className="font-mono">j</kbd>/<kbd className="font-mono">k</kbd> move,{" "}
              <kbd className="font-mono">x</kbd> select, <kbd className="font-mono">u</kbd>{" "}
              read/unread, <kbd className="font-mono">e</kbd> archive.
            </span>
          </p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <div
            role="group"
            aria-label="Show"
            id={tabsId}
            className="inline-flex h-9 rounded-md border border-line bg-surface p-0.5 shadow-xs"
          >
            {TABS.map((t) => (
              <button
                key={t.state}
                type="button"
                aria-pressed={state === t.state}
                onClick={() => show(t.state, highOnly)}
                className={cn(
                  "inline-flex items-center gap-1.5 rounded px-3 text-sm font-medium whitespace-nowrap text-muted transition-colors hover:text-ink",
                  state === t.state && "bg-beacon-soft text-ink",
                )}
              >
                {t.label}
                {t.state === "unread" && unread ? (
                  <span className="rounded-full bg-beacon px-1.5 text-[11px] text-surface tabular-nums">
                    {unread}
                  </span>
                ) : null}
              </button>
            ))}
          </div>
          <Button
            variant="outline"
            aria-pressed={highOnly}
            onClick={() => show(state, !highOnly)}
            className={cn(highOnly && "bg-beacon-soft hover:bg-beacon-soft")}
          >
            <Flag aria-hidden />
            High priority
          </Button>
        </div>
      </div>

      {query.isError && !query.data ? (
        <ErrorState
          title="The inbox could not be loaded."
          error={query.error}
          onRetry={() => void query.refetch()}
        />
      ) : query.isPending ? (
        <ul aria-busy aria-label="Loading inbox" className="flex flex-col gap-2">
          {Array.from({ length: 5 }, (_, i) => (
            <li
              key={i}
              data-testid="inbox-skeleton"
              className="flex items-center gap-4 rounded-xl border border-line bg-surface px-4 py-3.5"
            >
              <span aria-hidden className="size-9 animate-pulse rounded-lg bg-line" />
              <span className="flex flex-1 flex-col gap-2">
                <span aria-hidden className="h-3.5 w-56 animate-pulse rounded bg-line" />
                <span aria-hidden className="h-3 w-32 animate-pulse rounded bg-line" />
              </span>
            </li>
          ))}
        </ul>
      ) : items.length === 0 ? (
        <EmptyState
          title={empty.title}
          body={empty.body}
          action={
            state === "all" ? (
              <Button variant="outline" asChild>
                <Link to="/rules">
                  <Telescope aria-hidden />
                  Set up a rule
                </Link>
              </Button>
            ) : undefined
          }
        />
      ) : (
        <div
          className={cn(
            "grid items-start gap-4",
            opened && wide && "grid-cols-[minmax(0,1fr)_minmax(18rem,24rem)]",
          )}
        >
          <div className="flex min-w-0 flex-col gap-3">
            <SelectionBar
              count={`${items.length} item${items.length === 1 ? "" : "s"}`}
              selected={selected.size}
              total={items.length}
              onSelect={(all) => setSelected(new Set(all ? items.map((i) => i.id) : []))}
            >
              <BulkButton icon={MailOpen} label="Mark read" onClick={() => run("read", chosen)} />
              <BulkButton icon={Mail} label="Mark unread" onClick={() => run("unread", chosen)} />
              {state === "archived" ? (
                <BulkButton
                  icon={ArchiveRestore}
                  label="Unarchive"
                  onClick={() => run("unarchive", chosen)}
                />
              ) : (
                <BulkButton icon={Archive} label="Archive" onClick={() => run("archive", chosen)} />
              )}
            </SelectionBar>
            {groupByDay(items).map((g) => (
              <section key={g.label} aria-label={g.label} className="flex flex-col gap-1.5">
                <h2 className="px-1 text-xs font-semibold tracking-wide text-muted uppercase">
                  {g.label}
                </h2>
                <ul
                  aria-label={`Inbox, ${g.label}`}
                  className="overflow-hidden rounded-xl border border-line bg-surface"
                >
                  {g.items.map((it) => {
                    const idx = items.indexOf(it);
                    return (
                      <InboxRow
                        key={it.id}
                        item={it}
                        focused={cursor && idx === focus}
                        selected={selected.has(it.id)}
                        selecting={selected.size > 0}
                        active={it.id === opened?.id}
                        arrived={arrived.has(it.id)}
                        onToggle={() => toggle(it.id)}
                        onOpen={() => open(it)}
                        onFocus={() => {
                          setFocus(idx);
                          setCursor(true);
                        }}
                        onAction={(a) => run(a, [it.id])}
                      />
                    );
                  })}
                </ul>
              </section>
            ))}
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
          </div>
          {opened && wide && <InboxPanel item={opened} onClose={close} onAction={actOnOpened} />}
        </div>
      )}
      {/* Narrow screens: a modal dialog, which traps focus and gives it back on close. */}
      <Dialog
        open={dialogOpen}
        title={opened ? summarizeEvent(opened.event).text : ""}
        onClose={close}
        className="w-[calc(100vw-1.5rem)]"
      >
        {opened && !wide && (
          <>
            <DetailBody item={opened} onAction={actOnOpened} />
            <DialogActions>
              <Button variant="ghost" ref={closeRef} onClick={close}>
                Close
              </Button>
            </DialogActions>
          </>
        )}
      </Dialog>
    </div>
  );
}

function BulkButton({
  icon: Icon,
  label,
  onClick,
}: {
  icon: LucideIcon;
  label: string;
  onClick: () => void;
}) {
  return (
    <Button variant="ghost" size="sm" onClick={onClick}>
      <Icon aria-hidden />
      {label}
    </Button>
  );
}

function InboxRow({
  item,
  focused,
  selected,
  selecting,
  active,
  arrived,
  onToggle,
  onOpen,
  onFocus,
  onAction,
}: {
  item: InboxItem;
  focused: boolean;
  selected: boolean;
  selecting: boolean;
  active: boolean;
  arrived: boolean;
  onToggle: () => void;
  onOpen: () => void;
  onFocus: () => void;
  onAction: (a: InboxAction) => void;
}) {
  const s = summarizeEvent(item.event);
  const unread = !item.read_at;
  const at = new Date(item.created_at);
  return (
    <li
      data-unread={unread}
      data-focused={focused}
      className={cn(
        "group relative flex items-center gap-3 border-b border-line px-3 py-2.5 last:border-0",
        unread ? "bg-surface" : "bg-bg/40",
        selected && "bg-beacon-soft/50",
        active && "bg-beacon-soft",
        focused && "shadow-[inset_3px_0_0_var(--beacon)]",
        arrived && "animate-arrive",
      )}
    >
      <SelectTile
        label={s.text}
        selected={selected}
        selecting={selecting}
        onToggle={onToggle}
        className="size-9"
      >
        <KindIcon kind={item.watch.kind} className="size-9" />
      </SelectTile>
      <button
        id={rowId(item.id)}
        type="button"
        onClick={onOpen}
        onFocus={onFocus}
        aria-current={active || undefined}
        className="flex min-w-0 flex-1 items-center gap-3 rounded-md text-left outline-offset-4"
      >
        <span className="flex min-w-0 flex-1 flex-col gap-0.5">
          <span
            className={cn(
              "line-clamp-2 break-words sm:line-clamp-1",
              unread ? "font-semibold text-ink" : "text-muted",
            )}
          >
            {unread && <span className="sr-only">Unread: </span>}
            {s.text}
          </span>
          <span className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted">
            {item.priority === "high" && <PriorityBadge />}
            <span className={cn("rounded px-1.5 py-px ring-1 ring-inset", TONE_CLASS[s.tone])}>
              {s.badge}
            </span>
            <span className="truncate">{item.watch.name}</span>
          </span>
        </span>
        <time
          dateTime={item.created_at}
          title={formatAbsolute(at)}
          className="shrink-0 text-xs text-muted tabular-nums"
        >
          {formatRelative(at)}
        </time>
        <span
          aria-hidden
          className={cn("size-2 shrink-0 rounded-full", unread ? "bg-beacon" : "bg-transparent")}
        />
      </button>
      {/* Pinned to the row's right edge: the next row lands under the pointer at the same spot. */}
      <div
        role="group"
        aria-label={`Actions: ${s.text}`}
        className="absolute top-1/2 right-2 hidden -translate-y-1/2 items-center gap-0.5 rounded-md border border-line bg-surface p-0.5 shadow-xs group-hover:flex group-has-[:focus-visible]:flex"
      >
        <Button
          variant="ghost"
          size="icon"
          title={unread ? "Mark as read" : "Mark as unread"}
          aria-label={unread ? "Mark as read" : "Mark as unread"}
          onClick={() => onAction(unread ? "read" : "unread")}
        >
          {unread ? <MailOpen aria-hidden /> : <Mail aria-hidden />}
        </Button>
        <Button
          variant="ghost"
          size="icon"
          title={item.archived_at ? "Unarchive item" : "Archive item"}
          aria-label={item.archived_at ? "Unarchive item" : "Archive item"}
          onClick={() => onAction(item.archived_at ? "unarchive" : "archive")}
        >
          {item.archived_at ? <ArchiveRestore aria-hidden /> : <Archive aria-hidden />}
        </Button>
      </div>
    </li>
  );
}

// Payload lists of names, shown comma-separated and hidden when empty.
const LISTS = new Set(["cves", "labels"]);

function payloadRows(payload: Record<string, unknown>): [string, string][] {
  return Object.entries(payload)
    .filter(([k, v]) => k !== "url" && k !== "body" && !(LISTS.has(k) && isEmptyList(v)))
    .map(([k, v]) => [
      k,
      LISTS.has(k) && Array.isArray(v)
        ? v.join(", ")
        : typeof v === "string"
          ? v
          : JSON.stringify(v),
    ]);
}

const isEmptyList = (v: unknown) => Array.isArray(v) && v.length === 0;

function InboxPanel({
  item,
  onClose,
  onAction,
}: {
  item: InboxItem;
  onClose: () => void;
  onAction: (a: InboxAction) => void;
}) {
  const titleId = useId();
  return (
    <aside
      aria-labelledby={titleId}
      className="sticky top-[calc(var(--shell-top)+1rem)] flex max-h-[calc(100dvh-var(--shell-top)-2rem)] flex-col gap-4 overflow-y-auto rounded-xl border border-line bg-surface p-5"
    >
      <div className="flex items-start gap-3">
        <h2 id={titleId} className="sr-only">
          {summarizeEvent(item.event).text}
        </h2>
        <div className="flex-1">
          <DetailHeader item={item} />
        </div>
        <Button variant="ghost" size="icon" aria-label="Close details" onClick={onClose}>
          <X aria-hidden />
        </Button>
      </div>
      <DetailBody item={item} onAction={onAction} />
    </aside>
  );
}

function DetailHeader({ item }: { item: InboxItem }) {
  const s = summarizeEvent(item.event);
  return (
    <div className="flex items-start gap-3">
      <KindIcon kind={item.watch.kind} />
      <div className="flex min-w-0 flex-1 flex-col gap-1">
        <span
          className={cn("w-fit rounded px-1.5 py-px text-xs ring-1 ring-inset", TONE_CLASS[s.tone])}
        >
          {s.badge}
        </span>
        <p aria-hidden className="font-display text-lg leading-snug font-semibold break-words">
          {s.text}
        </p>
      </div>
    </div>
  );
}

function DetailBody({ item, onAction }: { item: InboxItem; onAction: (a: InboxAction) => void }) {
  const s = summarizeEvent(item.event);
  const rows = payloadRows(item.event.payload);
  const notes = item.event.payload.body;
  return (
    <div className="flex flex-col gap-4">
      {item.priority === "high" && <PriorityBadge />}
      <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-2 text-sm">
        <dt className="text-muted">Watch</dt>
        <dd className="min-w-0 truncate">
          <Link
            to={`/watches/${item.watch.id}`}
            className="font-medium underline-offset-4 hover:underline"
          >
            {item.watch.name}
          </Link>
        </dd>
        <dt className="text-muted">Event</dt>
        <dd className="font-mono text-xs">{item.event.type}</dd>
        <dt className="text-muted">Arrived</dt>
        <dd>
          <Timestamp at={item.created_at} />
        </dd>
        {item.event.occurred_at && (
          <>
            <dt className="text-muted">Upstream</dt>
            <dd>
              <Timestamp at={item.event.occurred_at} />
            </dd>
          </>
        )}
      </dl>
      {rows.length > 0 && (
        <section aria-label="Details" className="rounded-lg bg-bg p-3">
          <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1.5 text-xs">
            {rows.map(([k, v]) => (
              <div key={k} className="contents">
                <dt className="font-mono text-muted">{k}</dt>
                <dd className="min-w-0 font-mono break-words">{v}</dd>
              </div>
            ))}
          </dl>
        </section>
      )}
      {typeof notes === "string" && notes !== "" && (
        <details className="rounded-lg bg-bg p-3 text-xs">
          <summary className="cursor-pointer text-muted">Release notes</summary>
          <pre className="mt-2 font-mono break-words whitespace-pre-wrap">{notes}</pre>
        </details>
      )}
      <div className="flex flex-wrap gap-2">
        {s.url && (
          <Button variant="primary" asChild>
            <a href={s.url} target="_blank" rel="noreferrer noopener">
              <ExternalLink aria-hidden />
              Open upstream
            </a>
          </Button>
        )}
        <Button variant="outline" onClick={() => onAction(item.read_at ? "unread" : "read")}>
          {item.read_at ? <Mail aria-hidden /> : <MailOpen aria-hidden />}
          {item.read_at ? "Mark unread" : "Mark read"}
        </Button>
        <Button
          variant="outline"
          onClick={() => onAction(item.archived_at ? "unarchive" : "archive")}
        >
          {item.archived_at ? <ArchiveRestore aria-hidden /> : <Archive aria-hidden />}
          {item.archived_at ? "Unarchive" : "Archive"}
        </Button>
      </div>
    </div>
  );
}

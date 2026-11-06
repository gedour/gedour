import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ExternalLink, Play, Search } from "lucide-react";
import { Tabs } from "radix-ui";
import { useEffect, useId, useRef, useState } from "react";

import { getWatch, listWatchEvents, listWatchStates, runWatch } from "@/api/client";
import { ErrorState } from "@/components/ErrorState";
import { Timestamp } from "@/components/Timestamp";
import { Button } from "@/components/ui/button";
import { inputClass } from "@/components/ui/field";
import { StatusDot } from "@/components/WatchBits";
import { useToast } from "@/hooks/toast";
import { summarizeEvent, TONE_CLASS } from "@/lib/events";
import { problemText } from "@/lib/problems";
import { formatAbsolute } from "@/lib/time";
import { cn } from "@/lib/utils";
import {
  stateLine,
  type MRState,
  type PRState,
  type ReleaseState,
  type TagState,
} from "@/lib/watchState";
import { nextRunText } from "@/lib/watchStatus";
import type { StateSnapshot, Watch, WatchEvent } from "@/types/api";

const sectionClass = "flex flex-col gap-4 rounded-xl border border-line bg-surface p-5 sm:p-6";
const badge =
  "inline-flex items-center rounded-md px-2 py-0.5 text-xs font-medium ring-1 ring-inset";
// A run is at most 60 s on the worker, plus a 2 s poll: stop waiting for it after that.
const RUN_WAIT_MS = 90_000;

export function StatusCard({ watch }: { watch: Watch }) {
  const canWrite = watch.permission !== "ro";
  const client = useQueryClient();
  const toast = useToast();
  // last_run_at when "Run now" was accepted; that run is done once it moves.
  const [waitingFrom, setWaitingFrom] = useState<{ prev: string | null } | null>(null);
  const awaiting = waitingFrom !== null && watch.last_run_at === waitingFrom.prev;
  const running = watch.running || awaiting;
  useQuery({
    queryKey: ["watch", watch.id],
    queryFn: () => getWatch(watch.id),
    enabled: running,
    refetchInterval: running ? 2000 : false,
  });
  const waitTimer = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => clearTimeout(waitTimer.current), []);
  // Any finished run (ours or the schedule's) may have new events and a new state.
  const lastRun = useRef(watch.last_run_at);
  useEffect(() => {
    if (lastRun.current === watch.last_run_at) return;
    lastRun.current = watch.last_run_at;
    void client.invalidateQueries({ queryKey: ["watch-events", watch.id] });
    void client.invalidateQueries({ queryKey: ["watch-states", watch.id] });
  }, [client, watch.id, watch.last_run_at]);
  const run = useMutation({
    mutationFn: () => runWatch(watch.id),
    onSuccess: (w) => {
      setWaitingFrom({ prev: watch.last_run_at });
      clearTimeout(waitTimer.current);
      waitTimer.current = setTimeout(() => setWaitingFrom(null), RUN_WAIT_MS);
      client.setQueryData(["watch", w.id], w);
      toast(`Run of ${w.name} scheduled`);
    },
    onError: (err) => toast(`Could not run the watch: ${problemText(err)}`, "error"),
  });
  const busy = run.isPending || running;

  return (
    <section className={sectionClass} aria-labelledby="status-title">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h2 id="status-title" className="font-display text-lg font-semibold">
          Status
        </h2>
        {canWrite && (
          <Button
            variant="outline"
            disabled={busy || watch.paused}
            title={watch.paused ? "Resume the watch to run it" : undefined}
            onClick={() => run.mutate()}
          >
            <Play aria-hidden className={cn(busy && "animate-pulse")} />
            {run.isPending ? "Scheduling…" : running ? "Running…" : "Run now"}
          </Button>
        )}
      </div>
      <dl className="grid gap-x-6 gap-y-3 text-sm sm:grid-cols-[max-content_1fr]">
        <dt className="text-muted">Status</dt>
        <dd className="flex flex-wrap items-center gap-2">
          <StatusDot watch={watch} showLabel />
          {watch.consecutive_failures > 0 && (
            <span className="text-xs text-muted">
              {watch.consecutive_failures} failure{watch.consecutive_failures === 1 ? "" : "s"} in a
              row, retrying with backoff
            </span>
          )}
        </dd>
        <dt className="text-muted">Last run</dt>
        <dd>{watch.last_run_at ? <Timestamp at={watch.last_run_at} /> : "Never"}</dd>
        <dt className="text-muted">Next run</dt>
        <dd>
          {watch.paused && !watch.running ? (
            "Paused"
          ) : (
            <span title={formatAbsolute(new Date(watch.next_run_at))}>{nextRunText(watch)}</span>
          )}
        </dd>
        {watch.last_error && (
          <>
            <dt className="text-muted">
              {watch.last_status === "rate_limited" ? "Message" : "Last error"}
            </dt>
            <dd className="min-w-0">
              <details className="group">
                <summary className="cursor-pointer truncate font-mono text-xs text-rose-700 marker:text-muted dark:text-rose-300">
                  {watch.last_error.split("\n")[0]}
                </summary>
                <pre className="mt-2 max-h-64 overflow-auto rounded-lg bg-bg p-3 font-mono text-xs break-words whitespace-pre-wrap">
                  {watch.last_error}
                </pre>
              </details>
            </dd>
          </>
        )}
      </dl>
    </section>
  );
}

export function ActivityTabs({ watch }: { watch: Watch }) {
  return (
    <Tabs.Root defaultValue="events" className={sectionClass}>
      <Tabs.List aria-label="Activity" className="flex gap-1 border-b border-line">
        {(
          [
            ["events", "Events"],
            ["state", "State"],
          ] as const
        ).map(([value, label]) => (
          <Tabs.Trigger
            key={value}
            value={value}
            className="-mb-px border-b-2 border-transparent px-3 pb-2 font-display text-base font-semibold text-muted transition-colors hover:text-ink data-[state=active]:border-beacon data-[state=active]:text-ink"
          >
            {label}
          </Tabs.Trigger>
        ))}
      </Tabs.List>
      <Tabs.Content value="events">
        <EventsTimeline watch={watch} />
      </Tabs.Content>
      <Tabs.Content value="state">
        <StatePanel watch={watch} />
      </Tabs.Content>
    </Tabs.Root>
  );
}

function Skeleton({ rows = 3, label }: { rows?: number; label: string }) {
  return (
    <div aria-busy aria-label={label} className="flex flex-col gap-2">
      {Array.from({ length: rows }, (_, i) => (
        <span key={i} aria-hidden className="h-10 animate-pulse rounded-lg bg-line/60" />
      ))}
    </div>
  );
}

function Empty({ title, body }: { title: string; body: string }) {
  return (
    <div className="flex flex-col gap-1 rounded-lg border border-dashed border-line px-5 py-6">
      <p className="font-medium">{title}</p>
      <p className="text-sm text-muted">{body}</p>
    </div>
  );
}

export function EventsTimeline({ watch }: { watch: Watch }) {
  const query = useInfiniteQuery({
    queryKey: ["watch-events", watch.id],
    queryFn: ({ pageParam }) => listWatchEvents(watch.id, pageParam),
    initialPageParam: "",
    getNextPageParam: (last) => last.next_cursor || undefined,
  });
  if (query.isError) {
    return (
      <ErrorState
        title="Events could not be loaded."
        error={query.error}
        onRetry={() => void query.refetch()}
      />
    );
  }
  if (query.isPending) return <Skeleton label="Loading events" />;
  const events = query.data.pages.flatMap((p) => p.items);
  if (events.length === 0) {
    return watch.last_run_at ? (
      <Empty
        title="No changes yet"
        body="gedour checks on schedule and lists every change it finds here."
      />
    ) : (
      <Empty
        title="Waiting for the first run"
        body="The first run records the current state as a baseline; changes after it show up here."
      />
    );
  }
  const now = new Date();
  return (
    <div className="flex flex-col gap-3">
      <ol aria-label="Events" className="flex flex-col">
        {events.map((e) => (
          <EventItem key={e.id} event={e} now={now} />
        ))}
      </ol>
      {query.hasNextPage && (
        <Button
          variant="outline"
          className="self-start"
          disabled={query.isFetchingNextPage}
          onClick={() => void query.fetchNextPage()}
        >
          {query.isFetchingNextPage ? "Loading…" : "Load more"}
        </Button>
      )}
    </div>
  );
}

function EventItem({ event, now }: { event: WatchEvent; now: Date }) {
  const s = summarizeEvent(event);
  return (
    <li className="relative flex gap-3 border-l-2 border-line py-2.5 pl-4 last:pb-0">
      <span
        aria-hidden
        className="absolute top-4 -left-[5px] size-2 rounded-full bg-line ring-2 ring-surface"
      />
      <div className="flex min-w-0 flex-1 flex-col gap-1 sm:flex-row sm:items-center sm:gap-3">
        <span className={cn(badge, "w-fit shrink-0", TONE_CLASS[s.tone])}>{s.badge}</span>
        <span className="min-w-0 flex-1 font-medium break-words">{s.text}</span>
        <span className="flex shrink-0 items-center gap-2 text-xs text-muted">
          <Timestamp at={event.occurred_at ?? event.observed_at} now={now} />
          {s.url && (
            <a
              href={s.url}
              target="_blank"
              rel="noreferrer noopener"
              aria-label={`Open ${s.text} upstream`}
              className="rounded p-1 hover:bg-line/60 hover:text-ink"
            >
              <ExternalLink aria-hidden className="size-3.5" />
            </a>
          )}
        </span>
      </div>
    </li>
  );
}

export function StatePanel({ watch }: { watch: Watch }) {
  const query = useQuery({
    queryKey: ["watch-states", watch.id],
    queryFn: () => listWatchStates(watch.id),
  });
  if (query.isError) {
    return (
      <ErrorState
        title="The state could not be loaded."
        error={query.error}
        onRetry={() => void query.refetch()}
      />
    );
  }
  if (query.isPending) return <Skeleton label="Loading state" />;
  const [current] = query.data;
  if (!current) {
    return (
      <Empty
        title="No state recorded yet"
        body="The first run stores what the upstream looks like now."
      />
    );
  }
  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-col gap-2">
        <p className="text-xs text-muted">
          Current state, observed <Timestamp at={current.observed_at} />
        </p>
        <StateView kind={watch.kind} state={current.state} />
      </div>
      <StateHistory watch={watch} states={query.data} />
    </div>
  );
}

export function StateView({ kind, state }: { kind: Watch["kind"]; state: unknown }) {
  switch (kind) {
    case "github_repo":
    case "gitlab_release":
      return <ReleaseTable state={state as ReleaseState} />;
    case "github_pr":
      return <PRSummary state={state as PRState} />;
    case "gitlab_mr":
      return <MRSummary state={state as MRState} />;
    case "oci_tags":
      return <TagList state={state as TagState} />;
  }
}

function ReleaseTable({ state }: { state: ReleaseState }) {
  const releases = state.releases ?? [];
  if (releases.length === 0) return <p className="text-sm text-muted">No releases upstream.</p>;
  return (
    <div className="relative overflow-x-auto rounded-lg border border-line">
      <table className="w-full text-sm">
        <thead className="bg-bg/60 text-left text-xs text-muted">
          <tr>
            <th className="px-3 py-2 font-medium">Tag</th>
            <th className="px-3 py-2 font-medium">Name</th>
            <th className="px-3 py-2 font-medium">Published</th>
          </tr>
        </thead>
        <tbody>
          {releases.map((r) => (
            <tr key={r.tag} className="border-t border-line">
              <td className="px-3 py-2 font-mono whitespace-nowrap">
                {/^https?:\/\//.test(r.url) ? (
                  <a
                    href={r.url}
                    target="_blank"
                    rel="noreferrer noopener"
                    className="underline-offset-2 hover:underline"
                  >
                    {r.tag}
                  </a>
                ) : (
                  r.tag
                )}
                {r.prerelease && (
                  <span className={cn(badge, "ml-2", TONE_CLASS.change)}>pre-release</span>
                )}
                {r.draft && <span className={cn(badge, "ml-2", TONE_CLASS.neutral)}>draft</span>}
              </td>
              <td className="max-w-64 truncate px-3 py-2">{r.name}</td>
              <td className="px-3 py-2 whitespace-nowrap text-muted">
                {r.published_at ? <Timestamp at={r.published_at} /> : "—"}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

const TONE_OF: Record<string, keyof typeof TONE_CLASS> = {
  open: "change",
  opened: "change",
  merged: "good",
  closed: "bad",
  locked: "neutral",
  approved: "good",
  changes_requested: "bad",
  review_required: "change",
  success: "good",
  failure: "bad",
  failed: "bad",
  pending: "change",
  running: "change",
  none: "neutral",
};

function Pill({ label, value }: { label: string; value: string }) {
  return (
    <span className="flex flex-col gap-1">
      <span className="text-xs text-muted">{label}</span>
      <span className={cn(badge, "w-fit", TONE_CLASS[TONE_OF[value] ?? "neutral"])}>
        {value.replaceAll("_", " ")}
      </span>
    </span>
  );
}

function ChangeHeader({ title, url, prefix }: { title: string; url: string; prefix: string }) {
  return (
    <p className="font-medium">
      <span className="mr-2 font-mono text-muted">{prefix}</span>
      {/^https?:\/\//.test(url) ? (
        <a
          href={url}
          target="_blank"
          rel="noreferrer noopener"
          className="underline-offset-2 hover:underline"
        >
          {title}
        </a>
      ) : (
        title
      )}
    </p>
  );
}

function PRSummary({ state }: { state: PRState }) {
  return (
    <div className="flex flex-col gap-4 rounded-lg border border-line p-4">
      <ChangeHeader title={state.title} url={state.url} prefix={`#${state.number}`} />
      <div className="flex flex-wrap gap-6">
        <Pill label="State" value={state.draft ? `${state.state} (draft)` : state.state} />
        <Pill label="Review" value={state.review_decision} />
        <Pill label="CI" value={state.ci} />
        <span className="flex flex-col gap-1">
          <span className="text-xs text-muted">Head</span>
          <code className="font-mono text-xs">{state.head_sha.slice(0, 12)}</code>
        </span>
      </div>
    </div>
  );
}

function MRSummary({ state }: { state: MRState }) {
  const a = state.approvals;
  return (
    <div className="flex flex-col gap-4 rounded-lg border border-line p-4">
      <ChangeHeader title={state.title} url={state.url} prefix={`!${state.iid}`} />
      <div className="flex flex-wrap gap-6">
        <Pill label="State" value={state.draft ? `${state.state} (draft)` : state.state} />
        <span className="flex flex-col gap-1">
          <span className="text-xs text-muted">Approvals</span>
          <span className={cn(badge, "w-fit", a.approved ? TONE_CLASS.good : TONE_CLASS.neutral)}>
            {a.approved ? "approved" : "not approved"} · {a.count}
          </span>
        </span>
        <Pill label="Pipeline" value={state.pipeline} />
        <span className="flex flex-col gap-1">
          <span className="text-xs text-muted">Head</span>
          <code className="font-mono text-xs">{state.sha.slice(0, 12)}</code>
        </span>
      </div>
    </div>
  );
}

const TAG_LIMIT = 300;

function TagList({ state }: { state: TagState }) {
  const [q, setQ] = useState("");
  const id = useId();
  const tags = state.tags ?? [];
  const needle = q.trim().toLowerCase();
  // Newest-looking first: the collector sorts ascending.
  const shown = [...tags].reverse().filter((t) => t.toLowerCase().includes(needle));
  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <p className="text-sm">
          <span className="font-semibold">{state.count}</span> tags
          {state.highest && (
            <>
              {" "}
              · highest <span className="font-mono font-semibold">{state.highest}</span>
            </>
          )}
        </p>
        <div className="relative w-full sm:w-64">
          <label htmlFor={id} className="sr-only">
            Search tags
          </label>
          <Search
            aria-hidden
            className="pointer-events-none absolute top-2.5 left-2.5 size-4 text-muted"
          />
          <input
            id={id}
            type="search"
            value={q}
            placeholder="Search tags"
            onChange={(e) => setQ(e.target.value)}
            className={cn(inputClass, "pl-8")}
          />
        </div>
      </div>
      {shown.length === 0 ? (
        <p className="text-sm text-muted">No tag matches “{q}”.</p>
      ) : (
        <ul aria-label="Tags" className="flex flex-wrap gap-1.5">
          {shown.slice(0, TAG_LIMIT).map((t) => (
            <li
              key={t}
              className={cn(
                "rounded-md bg-bg px-2 py-0.5 font-mono text-xs ring-1 ring-line ring-inset",
                t === state.highest && "bg-beacon-soft ring-beacon/40",
              )}
            >
              {t}
            </li>
          ))}
        </ul>
      )}
      {shown.length > TAG_LIMIT && (
        <p className="text-xs text-muted">
          {shown.length - TAG_LIMIT} more; narrow the search to see them.
        </p>
      )}
    </div>
  );
}

function StateHistory({ watch, states }: { watch: Watch; states: StateSnapshot[] }) {
  return (
    <div className="flex flex-col gap-2">
      <h3 className="text-sm font-semibold">History</h3>
      <ol
        aria-label="State history"
        className="divide-y divide-line rounded-lg border border-line text-sm"
      >
        {states.map((s, i) => (
          <li key={s.id} className="flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2">
            <span className="w-28 shrink-0 text-xs text-muted">
              <Timestamp at={s.observed_at} />
            </span>
            <span className="min-w-0 flex-1">{stateLine(watch.kind, s.state)}</span>
            {i === 0 && <span className={cn(badge, TONE_CLASS.release)}>current</span>}
          </li>
        ))}
      </ol>
    </div>
  );
}

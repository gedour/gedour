import { keepPreviousData, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ArrowLeft,
  CircleAlert,
  CircleCheck,
  FlaskConical,
  Inbox,
  LoaderCircle,
} from "lucide-react";
import { useEffect, useId, useRef, useState, type FormEvent, type ReactNode } from "react";
import { Link, useNavigate, useParams } from "react-router";

import {
  ApiError,
  createRule,
  deleteRule,
  getRule,
  listChannels,
  listWatches,
  previewRule,
  updateRule,
} from "@/api/client";
import { ChannelIcon } from "@/components/ChannelBits";
import { channelTarget } from "@/lib/channels";
import { ErrorState } from "@/components/ErrorState";
import { FilterReference } from "@/components/FilterReference";
import { Button } from "@/components/ui/button";
import { Dialog, DialogActions } from "@/components/ui/dialog";
import { Field, inputClass } from "@/components/ui/field";
import { useToast } from "@/hooks/toast";
import { summarizeEvent, TONE_CLASS } from "@/lib/events";
import { FILTER_FIELDS, filterProblem, insertAt, marker, type FilterProblem } from "@/lib/filter";
import { fieldErrors, problemText, unplaced } from "@/lib/problems";
import { formatRelative } from "@/lib/time";
import { cn } from "@/lib/utils";
import { NotFound } from "@/pages/NotFound";
import type { PreviewItem, Rule } from "@/types/api";

/** /rules/new and /rules/:id. */
export function RuleFormPage() {
  const { id } = useParams();
  const query = useQuery({
    queryKey: ["rule", id],
    queryFn: () => getRule(id ?? ""),
    enabled: Boolean(id),
  });
  if (!id) return <RuleForm />;
  if (query.isPending) {
    return (
      <Page title="Edit rule">
        <p className="text-muted" aria-busy>
          Loading…
        </p>
      </Page>
    );
  }
  if (query.isError) {
    if (query.error instanceof ApiError && query.error.status === 404) return <NotFound />;
    return (
      <Page title="Edit rule">
        <ErrorState
          title="The rule could not be loaded."
          error={query.error}
          onRetry={() => void query.refetch()}
        />
      </Page>
    );
  }
  return <RuleForm key={query.data.id} initial={query.data} />;
}

function Page({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="mx-auto flex w-full max-w-4xl flex-col gap-6 px-5 py-8 md:px-10 md:py-12">
      <title>{`${title} – gedour`}</title>
      <Link
        to="/rules"
        className="inline-flex w-fit items-center gap-1.5 text-sm text-muted hover:text-ink"
      >
        <ArrowLeft aria-hidden className="size-4" />
        Rules
      </Link>
      <h1 className="font-display text-3xl font-semibold tracking-tight">{title}</h1>
      {children}
    </div>
  );
}

function useDebounced<T>(value: T, ms: number): T {
  const [v, setV] = useState(value);
  useEffect(() => {
    const t = setTimeout(() => setV(value), ms);
    return () => clearTimeout(t);
  }, [value, ms]);
  return v;
}

const sectionClass = "flex flex-col gap-4 rounded-xl border border-line bg-surface p-5 sm:p-6";

function RuleForm({ initial }: { initial?: Rule }) {
  const [name, setName] = useState(initial?.name ?? "");
  const [watchId, setWatchId] = useState(initial?.watch_id ?? "");
  const [filter, setFilter] = useState(initial?.filter ?? "");
  const [enabled, setEnabled] = useState(initial?.enabled ?? true);
  const [priority, setPriority] = useState<Rule["priority"]>(initial?.priority ?? "normal");
  const [channelIds, setChannelIds] = useState<string[]>(initial?.channel_ids ?? []);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [showPreview, setShowPreview] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const watches = useQuery({ queryKey: ["watches", {}], queryFn: () => listWatches() });
  const channels = useQuery({ queryKey: ["channels"], queryFn: listChannels });
  const client = useQueryClient();
  const navigate = useNavigate();
  const toast = useToast();
  const ids = { filter: useId(), fields: useId() };
  const area = useRef<HTMLTextAreaElement>(null);

  // Live compile: the preview endpoint compiles first, so its 400 is the editor's feedback.
  const debounced = useDebounced(filter.trim(), 350);
  const preview = useQuery({
    queryKey: ["rule-preview", watchId, debounced],
    queryFn: () => previewRule(debounced, watchId),
    placeholderData: keepPreviousData,
    retry: false,
  });
  const live = filterProblem(preview.error);
  // An error with no filter problem (5xx, 429, network): the filter could not be checked at all.
  const unchecked = preview.isError && !live;
  const settling = debounced !== filter.trim() || preview.isFetching;

  const save = useMutation({
    mutationFn: () => {
      const body = { name, filter, enabled, priority, channel_ids: channelIds };
      return initial
        ? updateRule(initial.id, { ...body, watch_id: watchId })
        : createRule({ ...body, watch_id: watchId || undefined });
    },
    onSuccess: (r) => {
      void client.invalidateQueries({ queryKey: ["rules"] });
      client.setQueryData(["rule", r.id], r);
      toast(`Saved ${r.name}`);
      void navigate("/rules");
    },
    onError: (err) => setErrors(fieldErrors(err)),
  });
  const del = useMutation({
    mutationFn: () => deleteRule(initial?.id ?? ""),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ["rules"] });
      toast(`Deleted ${initial?.name ?? "the rule"}`);
      void navigate("/rules");
    },
    onError: (err) => toast(`Could not delete the rule: ${problemText(err)}`, "error"),
  });

  // The caret goes after an inserted field once React has rendered the new text.
  const caret = useRef<number | null>(null);
  useEffect(() => {
    const el = area.current;
    if (caret.current === null || !el) return;
    el.focus();
    el.setSelectionRange(caret.current, caret.current);
    caret.current = null;
  }, [filter]);
  const insert = (text: string) => {
    const el = area.current;
    const next = insertAt(
      filter,
      el?.selectionStart ?? filter.length,
      el?.selectionEnd ?? filter.length,
      text,
    );
    caret.current = next.caret;
    setFilter(next.value);
  };
  const submit = (e: FormEvent) => {
    e.preventDefault();
    setErrors({});
    save.mutate();
  };
  const saveProblem = filterProblem(save.error);
  const problem = live ?? saveProblem;
  const watchList = watches.data ?? [];

  return (
    <Page title={initial ? `Edit ${initial.name}` : "New rule"}>
      <form noValidate onSubmit={submit} aria-label="Rule" className="flex flex-col gap-6">
        {unplaced(errors, ["name", "watch_id", "priority", "filter", "channel_ids"]).map((m) => (
          <p key={m} role="alert" className="text-sm text-rose-700 dark:text-rose-300">
            {m}
          </p>
        ))}
        <section className={sectionClass} aria-label="Scope">
          <Field label="Name" error={errors.name}>
            {(p) => (
              <input
                {...p}
                value={name}
                maxLength={200}
                onChange={(e) => setName(e.target.value)}
                className={inputClass}
              />
            )}
          </Field>
          <Field
            label="Watches"
            error={errors.watch_id}
            hint="All watches includes those shared with you, as long as they are."
          >
            {(p) => (
              <select
                {...p}
                value={watchId}
                onChange={(e) => setWatchId(e.target.value)}
                className={inputClass}
              >
                <option value="">All watches</option>
                {watchList.map((w) => (
                  <option key={w.id} value={w.id}>
                    {w.name}
                    {w.permission === "owner" ? "" : ` (shared by ${w.owner_name})`}
                  </option>
                ))}
              </select>
            )}
          </Field>
          <Field
            label="Priority"
            error={errors.priority}
            hint="High marks the inbox items of new matching events. Existing items keep their level."
          >
            {(p) => (
              <select
                {...p}
                value={priority}
                onChange={(e) => setPriority(e.target.value as Rule["priority"])}
                className={inputClass}
              >
                <option value="normal">Normal</option>
                <option value="high">High</option>
              </select>
            )}
          </Field>
          <label className="inline-flex w-fit items-center gap-2 text-sm">
            <input
              type="checkbox"
              checked={enabled}
              onChange={(e) => setEnabled(e.target.checked)}
              className="size-4 accent-beacon"
            />
            Enabled
          </label>
        </section>

        <section className={sectionClass} aria-labelledby={`${ids.filter}-title`}>
          <h2 id={`${ids.filter}-title`} className="font-display text-lg font-semibold">
            Filter
          </h2>
          <div className="flex flex-col gap-1.5">
            <label htmlFor={ids.filter} className="text-sm font-medium">
              Expression
            </label>
            <textarea
              id={ids.filter}
              ref={area}
              rows={3}
              spellCheck={false}
              value={filter}
              maxLength={2000}
              placeholder={'event.type == "release.published" && !event.payload.prerelease'}
              aria-invalid={problem ? true : undefined}
              aria-describedby={`${ids.filter}-status ${ids.fields}`}
              onChange={(e) => {
                setFilter(e.target.value);
                // A save error describes the old text.
                save.reset();
              }}
              className={cn(inputClass, "h-auto min-h-20 resize-y py-2 font-mono leading-relaxed")}
            />
            <FilterStatus
              id={`${ids.filter}-status`}
              src={filter}
              problem={problem}
              settling={settling}
              unchecked={unchecked}
            />
          </div>
          <div id={ids.fields} className="flex flex-col gap-2">
            <p className="text-xs font-medium text-muted">Available fields (click to insert)</p>
            <ul className="flex flex-wrap gap-1.5">
              {FILTER_FIELDS.map((f) => (
                <li key={f.expr}>
                  <button
                    type="button"
                    title={f.hint}
                    onClick={() => insert(f.expr)}
                    className="rounded-md bg-bg px-2 py-1 font-mono text-xs ring-1 ring-line ring-inset hover:ring-beacon"
                  >
                    {f.expr}
                  </button>
                </li>
              ))}
            </ul>
            <FilterReference />
          </div>
          <div className="flex flex-col gap-3">
            <Button
              type="button"
              variant="outline"
              className="self-start"
              onClick={() => setShowPreview(true)}
              disabled={Boolean(live) || unchecked}
            >
              <FlaskConical aria-hidden />
              Test against last 20 events
            </Button>
            {showPreview && !live && (
              <PreviewTable
                items={preview.data}
                loading={preview.isPending}
                error={preview.isError ? preview.error : null}
              />
            )}
          </div>
        </section>

        <section className={sectionClass} aria-labelledby={`${ids.filter}-channels`}>
          <h2 id={`${ids.filter}-channels`} className="font-display text-lg font-semibold">
            Send to
          </h2>
          <p className="inline-flex items-center gap-2 text-sm text-muted">
            <Inbox aria-hidden className="size-4" />
            Your inbox always gets matching events.
          </p>
          {errors.channel_ids && (
            <p className="text-sm text-rose-700 dark:text-rose-300">{errors.channel_ids}</p>
          )}
          {channels.data?.length === 0 ? (
            <p className="text-sm">
              No channels yet.{" "}
              <Link to="/channels" className="font-medium underline underline-offset-4">
                Add one
              </Link>{" "}
              to also get Telegram, Nextcloud Talk, email or webhook messages.
            </p>
          ) : (
            <fieldset className="flex min-w-0 flex-col gap-2">
              <legend className="sr-only">Channels</legend>
              {(channels.data ?? []).map((c) => (
                <label
                  key={c.id}
                  className="flex items-center gap-3 rounded-lg border border-line px-3 py-2 text-sm has-checked:border-beacon/60 has-checked:bg-beacon-soft/40"
                >
                  <input
                    type="checkbox"
                    checked={channelIds.includes(c.id)}
                    onChange={(e) =>
                      setChannelIds((cur) =>
                        e.target.checked ? [...cur, c.id] : cur.filter((x) => x !== c.id),
                      )
                    }
                    className="size-4 shrink-0 accent-beacon"
                  />
                  <ChannelIcon type={c.type} className="shrink-0 text-muted" />
                  <span className="min-w-0 truncate font-medium">{c.name}</span>
                  <span className="min-w-0 truncate text-xs text-muted">{channelTarget(c)}</span>
                </label>
              ))}
            </fieldset>
          )}
        </section>

        <div className="flex flex-wrap items-center gap-2">
          <Button type="submit" variant="primary" disabled={save.isPending}>
            {save.isPending ? "Saving…" : initial ? "Save rule" : "Create rule"}
          </Button>
          <Button variant="ghost" asChild>
            <Link to="/rules">Cancel</Link>
          </Button>
          {initial && (
            <Button
              type="button"
              variant="ghost"
              className="ml-auto text-rose-700 dark:text-rose-300"
              onClick={() => setDeleting(true)}
            >
              Delete rule
            </Button>
          )}
        </div>
      </form>
      <Dialog
        open={deleting}
        title={`Delete ${initial?.name ?? ""}?`}
        onClose={() => setDeleting(false)}
      >
        <p className="text-sm text-muted">
          Events stop going through it. Its past deliveries stay listed.
        </p>
        <DialogActions>
          <Button variant="ghost" onClick={() => setDeleting(false)}>
            Cancel
          </Button>
          <Button variant="danger" disabled={del.isPending} onClick={() => del.mutate()}>
            Delete
          </Button>
        </DialogActions>
      </Dialog>
    </Page>
  );
}

/** Compile state under the editor: checking, valid, or the error with its position marked. */
export function FilterStatus({
  id,
  src,
  problem,
  settling,
  unchecked = false,
}: {
  id: string;
  src: string;
  problem: FilterProblem | null;
  settling: boolean;
  unchecked?: boolean;
}) {
  if (problem) {
    const m = marker(src, problem);
    return (
      <div
        id={id}
        role="alert"
        className="flex flex-col gap-1.5 text-sm text-rose-700 dark:text-rose-300"
      >
        <p className="inline-flex items-start gap-1.5">
          <CircleAlert aria-hidden className="mt-0.5 size-4 shrink-0" />
          {problem.message}
        </p>
        {m && (
          <pre
            data-testid="filter-marker"
            aria-label={`Line ${m.line}, column ${m.before.length + 1}`}
            className="relative overflow-x-auto rounded-md bg-rose-500/8 px-3 py-2 font-mono text-xs leading-relaxed whitespace-pre text-ink"
          >
            {m.before}
            <mark className="bg-transparent text-rose-700 underline decoration-rose-600 decoration-wavy decoration-2 underline-offset-4 dark:text-rose-300">
              {m.at || " "}
            </mark>
            {m.after}
            {"\n"}
            <span aria-hidden className="text-rose-700 dark:text-rose-300">
              {" ".repeat(m.before.length)}^
            </span>
          </pre>
        )}
      </div>
    );
  }
  return (
    <p id={id} className="inline-flex items-center gap-1.5 text-xs text-muted" aria-live="polite">
      {settling ? (
        <>
          <LoaderCircle aria-hidden className="size-3.5 animate-spin" />
          Checking…
        </>
      ) : unchecked ? (
        <>
          <CircleAlert aria-hidden className="size-3.5 text-amber-600" />
          Could not check the filter right now; it is checked again on save.
        </>
      ) : (
        <>
          <CircleCheck aria-hidden className="size-3.5 text-emerald-600" />
          {src.trim() ? "Valid filter" : "Empty: every event matches"}
        </>
      )}
    </p>
  );
}

export function PreviewTable({
  items,
  loading,
  error,
}: {
  items: PreviewItem[] | undefined;
  loading: boolean;
  error: Error | null;
}) {
  if (error)
    return (
      <p role="alert" className="text-sm text-rose-700 dark:text-rose-300">
        {problemText(error)}
      </p>
    );
  if (loading || !items)
    return (
      <p className="text-sm text-muted" aria-busy>
        Evaluating…
      </p>
    );
  if (items.length === 0) {
    return (
      <p className="text-sm text-muted">No events yet in this scope: nothing to test against.</p>
    );
  }
  const matched = items.filter((i) => i.matched).length;
  return (
    <div className="flex flex-col gap-2">
      <p className="text-sm">
        <strong>{matched}</strong> of {items.length} recent events match.
      </p>
      <div className="relative overflow-x-auto rounded-lg border border-line">
        <table className="w-full min-w-[560px] text-left text-sm">
          <caption className="sr-only">Preview against recent events</caption>
          <thead className="border-b border-line text-xs tracking-wide text-muted uppercase">
            <tr>
              <th scope="col" className="px-3 py-2 font-medium">
                Event
              </th>
              <th scope="col" className="px-3 py-2 font-medium">
                Watch
              </th>
              <th scope="col" className="px-3 py-2 font-medium">
                Seen
              </th>
              <th scope="col" className="px-3 py-2 font-medium">
                Result
              </th>
            </tr>
          </thead>
          <tbody>
            {items.map((it) => {
              const s = summarizeEvent(it.event);
              return (
                <tr
                  key={it.event.id}
                  className={cn(
                    "border-b border-line last:border-0",
                    it.matched && "bg-emerald-500/5",
                  )}
                >
                  <td className="px-3 py-2">
                    <span className="flex items-center gap-2">
                      <span
                        className={cn(
                          "rounded px-1.5 py-px text-xs ring-1 ring-inset",
                          TONE_CLASS[s.tone],
                        )}
                      >
                        {s.badge}
                      </span>
                      <span className="truncate">{s.text}</span>
                    </span>
                  </td>
                  <td className="px-3 py-2 text-muted">{it.watch.name}</td>
                  <td className="px-3 py-2 text-xs whitespace-nowrap text-muted">
                    {formatRelative(new Date(it.event.observed_at))}
                  </td>
                  <td className="px-3 py-2 whitespace-nowrap">
                    {it.error ? (
                      <span
                        title={it.error}
                        className="inline-flex items-center gap-1 text-amber-700 dark:text-amber-300"
                      >
                        <CircleAlert aria-hidden className="size-4" />
                        Error
                      </span>
                    ) : it.matched ? (
                      <span className="inline-flex items-center gap-1 text-emerald-700 dark:text-emerald-300">
                        <span aria-hidden>✓</span> Match
                      </span>
                    ) : (
                      <span className="inline-flex items-center gap-1 text-muted">
                        <span aria-hidden>✗</span> No match
                      </span>
                    )}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </div>
  );
}

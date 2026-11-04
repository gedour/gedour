import { useQuery } from "@tanstack/react-query";
import { CircleAlert, CircleCheck, Clock, Globe, LoaderCircle, Skull } from "lucide-react";

import { previewRule } from "@/api/client";
import { inputClass } from "@/components/ui/field";
import { summarizeEvent } from "@/lib/events";
import { problemText } from "@/lib/problems";
import { SAMPLES, type SampleKey } from "@/lib/scripts";
import { cn } from "@/lib/utils";
import type { DryRun } from "@/types/api";

/** Recent events of the watches the caller can read (an empty filter matches all), then built-in samples. */
export function SamplePicker({
  id,
  value,
  onChange,
}: {
  id: string;
  value: SampleKey;
  onChange: (key: SampleKey) => void;
}) {
  const recent = useQuery({
    queryKey: ["script-samples"],
    queryFn: () => previewRule("", ""),
    staleTime: 60_000,
    retry: false,
  });
  const events = recent.data ?? [];
  return (
    <select
      id={id}
      value={value}
      onChange={(e) => onChange(e.target.value as SampleKey)}
      className={cn(inputClass, "w-full sm:w-72")}
    >
      <optgroup label="Built-in samples">
        {SAMPLES.map((s, i) => (
          <option key={s.label} value={`sample:${i}`}>
            {s.label}
          </option>
        ))}
      </optgroup>
      {events.length > 0 && (
        <optgroup label="Recent events">
          {events.map((it) => (
            <option key={it.event.id} value={`event:${it.event.id}`}>
              {it.watch.name}: {summarizeEvent(it.event).text}
            </option>
          ))}
        </optgroup>
      )}
    </select>
  );
}

const OUTCOME = {
  ok: { label: "Returned", icon: CircleCheck, className: "text-emerald-700 dark:text-emerald-300" },
  error: { label: "Threw", icon: CircleAlert, className: "text-rose-700 dark:text-rose-300" },
  timeout: { label: "Timed out", icon: Clock, className: "text-amber-700 dark:text-amber-300" },
  killed: { label: "Killed", icon: Skull, className: "text-rose-700 dark:text-rose-300" },
} as const;

const headingClass = "text-xs font-medium tracking-wide text-muted uppercase";
const blockClass =
  "max-h-64 overflow-auto rounded-md bg-bg px-3 py-2 font-mono text-xs leading-relaxed break-words whitespace-pre-wrap";

/** Logs, result and the recorded fetches of a dry run. */
export function ScriptResults({
  run,
  pending,
  error,
}: {
  run: DryRun | undefined;
  pending: boolean;
  error: Error | null;
}) {
  if (pending)
    return (
      <p className="inline-flex items-center gap-2 text-sm text-muted" aria-busy>
        <LoaderCircle aria-hidden className="size-4 animate-spin" />
        Running in the sandbox…
      </p>
    );
  if (error)
    return (
      <p role="alert" className="text-sm text-rose-700 dark:text-rose-300">
        {problemText(error)}
      </p>
    );
  if (!run)
    return (
      <p className="text-sm text-muted">
        Pick an event and run: logs, the returned value and every <code>http.fetch</code> show here.
        Nothing is sent.
      </p>
    );
  const o = OUTCOME[run.outcome];
  const fetches = run.fetches ?? [];
  const logs = run.logs ?? [];
  return (
    <div className="flex flex-col gap-4" data-testid="script-results">
      <p
        className={cn("inline-flex flex-wrap items-center gap-2 text-sm font-medium", o.className)}
      >
        <o.icon aria-hidden className="size-4" />
        {o.label} <span className="font-normal text-muted">in {run.duration_ms} ms</span>
      </p>
      {run.error && (
        <pre role="alert" className={cn(blockClass, "text-rose-800 dark:text-rose-200")}>
          {run.error}
        </pre>
      )}
      <section aria-label="Logs" className="flex flex-col gap-1.5">
        <h3 className={headingClass}>Logs</h3>
        {logs.length === 0 ? (
          <p className="text-sm text-muted">No log line.</p>
        ) : (
          <ol className={cn(blockClass, "list-none")}>
            {logs.map((l, i) => (
              <li key={i}>{l}</li>
            ))}
          </ol>
        )}
      </section>
      {run.result && (
        <section aria-label="Result" className="flex flex-col gap-1.5">
          <h3 className={headingClass}>Returned</h3>
          <pre className={blockClass}>{run.result}</pre>
        </section>
      )}
      <section aria-label="Fetches" className="flex flex-col gap-1.5">
        <h3 className={headingClass}>http.fetch calls (recorded, not sent)</h3>
        {fetches.length === 0 ? (
          <p className="text-sm text-muted">None.</p>
        ) : (
          <ul className="flex flex-col gap-2">
            {fetches.map((f, i) => (
              <li key={i} className="flex flex-col gap-1 rounded-md border border-line px-3 py-2">
                <span className="flex min-w-0 items-center gap-2 font-mono text-xs">
                  <Globe aria-hidden className="size-3.5 shrink-0 text-muted" />
                  <span className="font-semibold">{f.method}</span>
                  <span className="truncate">{f.url}</span>
                </span>
                <span className="font-mono text-xs break-all text-muted">
                  {Object.entries(f.headers ?? {})
                    .map(([k, v]) => `${k}: ${v}`)
                    .join(" · ") || "no headers"}{" "}
                  · {f.body_size} B body
                </span>
              </li>
            ))}
          </ul>
        )}
      </section>
    </div>
  );
}

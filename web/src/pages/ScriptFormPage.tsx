import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowLeft, CircleAlert, Play, Save } from "lucide-react";
import { useId, useState, type FormEvent, type ReactNode } from "react";
import { Link, useNavigate, useParams } from "react-router";

import {
  ApiError,
  createScript,
  deleteScript,
  dryRunScript,
  getScript,
  updateScript,
} from "@/api/client";
import { ErrorState } from "@/components/ErrorState";
import { ScriptEditor } from "@/components/ScriptEditor";
import { SamplePicker, ScriptResults } from "@/components/ScriptRun";
import { Button } from "@/components/ui/button";
import { Dialog, DialogActions } from "@/components/ui/dialog";
import { Field, inputClass } from "@/components/ui/field";
import { useToast } from "@/hooks/toast";
import { filterProblem } from "@/lib/filter";
import { fieldErrors, problemText, unplaced } from "@/lib/problems";
import { dryRunEventOf, STARTER_SOURCE, type SampleKey } from "@/lib/scripts";
import { NotFound } from "@/pages/NotFound";
import type { Script } from "@/types/api";

/** /scripts/new and /scripts/:id. */
export function ScriptFormPage() {
  const { id } = useParams();
  const query = useQuery({
    queryKey: ["script", id],
    queryFn: () => getScript(id ?? ""),
    enabled: Boolean(id),
  });
  if (!id) return <ScriptForm />;
  if (query.isPending) {
    return (
      <Page title="Edit script">
        <p className="text-muted" aria-busy>
          Loading…
        </p>
      </Page>
    );
  }
  if (query.isError) {
    if (query.error instanceof ApiError && query.error.status === 404) return <NotFound />;
    return (
      <Page title="Edit script">
        <ErrorState
          title="The script could not be loaded."
          error={query.error}
          onRetry={() => void query.refetch()}
        />
      </Page>
    );
  }
  return <ScriptForm key={query.data.id} initial={query.data} />;
}

function Page({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="mx-auto flex w-full max-w-5xl flex-col gap-6 px-5 py-8 md:px-10 md:py-12">
      <title>{`${title} – gedour`}</title>
      <Link
        to="/scripts"
        className="inline-flex w-fit items-center gap-1.5 text-sm text-muted hover:text-ink"
      >
        <ArrowLeft aria-hidden className="size-4" />
        Scripts
      </Link>
      <h1 className="font-display text-3xl font-semibold tracking-tight">{title}</h1>
      {children}
    </div>
  );
}

const sectionClass = "flex flex-col gap-4 rounded-xl border border-line bg-surface p-5 sm:p-6";

function ScriptForm({ initial }: { initial?: Script }) {
  const [name, setName] = useState(initial?.name ?? "");
  const [source, setSource] = useState(initial?.source ?? STARTER_SOURCE);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [sample, setSample] = useState<SampleKey>("sample:0");
  const [deleting, setDeleting] = useState(false);
  const client = useQueryClient();
  const navigate = useNavigate();
  const toast = useToast();
  const ids = { status: useId(), sample: useId(), results: useId() };

  const save = useMutation({
    mutationFn: () =>
      initial ? updateScript(initial.id, { name, source }) : createScript({ name, source }),
    onSuccess: (s) => {
      void client.invalidateQueries({ queryKey: ["scripts"] });
      client.setQueryData(["script", s.id], s);
      toast(`Saved ${s.name}`);
      if (!initial) void navigate(`/scripts/${s.id}`, { replace: true });
    },
    onError: (err) => setErrors(fieldErrors(err)),
  });
  // Always the editor's text, saved or not: the run shows what the next save would do.
  const run = useMutation({
    mutationFn: () => dryRunScript({ source }, dryRunEventOf(sample)),
  });
  const problem =
    filterProblem(save.error, "body.source") ?? filterProblem(run.error, "body.source");
  const doSave = () => {
    if (save.isPending) return;
    setErrors({});
    save.mutate();
  };
  const submit = (e: FormEvent) => {
    e.preventDefault();
    doSave();
  };
  const edit = (next: string) => {
    setSource(next);
    // An error describes the old text.
    if (save.error) save.reset();
    if (run.error) run.reset();
  };

  return (
    <Page title={initial ? `Edit ${initial.name}` : "New script"}>
      <form noValidate onSubmit={submit} aria-label="Script" className="flex flex-col gap-6">
        {unplaced(errors, ["name", "source"]).map((m) => (
          <p key={m} role="alert" className="text-sm text-rose-700 dark:text-rose-300">
            {m}
          </p>
        ))}
        <section className={sectionClass} aria-label="Code">
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
          <div className="flex flex-col gap-1.5">
            <span className="text-sm font-medium">Source</span>
            <ScriptEditor
              value={source}
              onChange={edit}
              problem={problem}
              label="Source"
              describedBy={ids.status}
              onSave={doSave}
            />
            {problem ? (
              <p
                id={ids.status}
                role="alert"
                className="inline-flex items-start gap-1.5 text-sm text-rose-700 dark:text-rose-300"
              >
                <CircleAlert aria-hidden className="mt-0.5 size-4 shrink-0" />
                {problem.message}
              </p>
            ) : (
              <p id={ids.status} className="text-xs text-muted">
                Define <code className="font-mono">function main(event, ctx)</code>; it may call{" "}
                <code className="font-mono">log(…)</code> and{" "}
                <code className="font-mono">http.fetch(url, options)</code>. Ctrl+Space completes,
                Ctrl+S saves. 64 KiB at most.
              </p>
            )}
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <Button type="submit" variant="primary" disabled={save.isPending}>
              <Save aria-hidden />
              {save.isPending ? "Saving…" : initial ? "Save script" : "Create script"}
            </Button>
            <Button variant="ghost" asChild>
              <Link to="/scripts">Cancel</Link>
            </Button>
            {initial && (
              <Button
                type="button"
                variant="ghost"
                className="ml-auto text-rose-700 dark:text-rose-300"
                onClick={() => setDeleting(true)}
              >
                Delete script
              </Button>
            )}
          </div>
        </section>
      </form>

      <section className={sectionClass} aria-labelledby={ids.results}>
        <div className="flex flex-wrap items-end justify-between gap-3">
          <div className="flex flex-col gap-1">
            <h2 id={ids.results} className="font-display text-lg font-semibold">
              Dry run
            </h2>
            <p className="text-sm text-muted">
              Runs the code above in the sandbox; <code className="font-mono">http.fetch</code> is
              recorded and answers 200 with an empty body.
            </p>
          </div>
          <div className="flex w-full flex-col gap-2 sm:w-auto sm:flex-row sm:items-end">
            <div className="flex flex-col gap-1">
              <label htmlFor={ids.sample} className="text-xs font-medium text-muted">
                Event
              </label>
              <SamplePicker id={ids.sample} value={sample} onChange={setSample} />
            </div>
            <Button
              type="button"
              variant="outline"
              disabled={run.isPending}
              onClick={() => run.mutate()}
            >
              <Play aria-hidden />
              Run with sample event
            </Button>
          </div>
        </div>
        <div aria-live="polite">
          <ScriptResults
            run={run.data}
            pending={run.isPending}
            error={run.isError && !problem ? run.error : null}
          />
        </div>
      </section>
      {initial && (
        <DeleteScriptDialog script={initial} open={deleting} onClose={() => setDeleting(false)} />
      )}
    </Page>
  );
}

function DeleteScriptDialog({
  script,
  open,
  onClose,
}: {
  script: Script;
  open: boolean;
  onClose: () => void;
}) {
  const [inUse, setInUse] = useState<string[] | null>(null);
  const [error, setError] = useState("");
  const client = useQueryClient();
  const navigate = useNavigate();
  const toast = useToast();
  const close = () => {
    setInUse(null);
    setError("");
    onClose();
  };
  const del = useMutation({
    mutationFn: () => deleteScript(script.id),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ["scripts"] });
      toast(`Deleted ${script.name}`);
      void navigate("/scripts");
    },
    onError: (err) => {
      if (err instanceof ApiError && err.status === 409)
        setInUse(err.errors.map((e) => String(e.value)));
      else setError(problemText(err));
    },
  });
  return (
    <Dialog open={open} title={`Delete ${script.name}?`} onClose={close}>
      {inUse ? (
        <div role="alert" className="flex flex-col gap-2 text-sm">
          <p>These channels still run it:</p>
          <ul className="list-disc pl-5 font-medium">
            {inUse.map((n) => (
              <li key={n}>{n}</li>
            ))}
          </ul>
          <p className="text-muted">Delete them or point them at another script first.</p>
        </div>
      ) : (
        <p className="text-sm text-muted">Its source is erased; the audit log keeps its hash.</p>
      )}
      {error && (
        <p role="alert" className="text-sm text-rose-700 dark:text-rose-300">
          {error}
        </p>
      )}
      <DialogActions>
        <Button variant="ghost" onClick={close}>
          {inUse ? "Close" : "Cancel"}
        </Button>
        {!inUse && (
          <Button variant="danger" disabled={del.isPending} onClick={() => del.mutate()}>
            Delete
          </Button>
        )}
      </DialogActions>
    </Dialog>
  );
}

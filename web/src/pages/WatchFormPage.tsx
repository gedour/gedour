import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowLeft, Link2, Sparkles } from "lucide-react";
import { useId, useState, type FormEvent, type ReactNode } from "react";
import { Link, useNavigate, useParams } from "react-router";

import { ApiError, createWatch, getWatch, listCredentials, updateWatch } from "@/api/client";
import { ErrorState } from "@/components/ErrorState";
import { Button } from "@/components/ui/button";
import { Field, inputClass } from "@/components/ui/field";
import { useToast } from "@/hooks/toast";
import { fieldErrors, problemText, unplaced } from "@/lib/problems";
import {
  credentialHost,
  describeSchedule,
  fromConfig,
  FIELDS,
  KINDS,
  kindInfo,
  SCHEDULE_PRESETS,
  targetSummary,
  toConfig,
  type FormValues,
} from "@/lib/watchKinds";
import { recognize } from "@/lib/watchUrl";
import { cn } from "@/lib/utils";
import { NotFound } from "@/pages/NotFound";
import type { Watch, WatchKind } from "@/types/api";

/** /watches/new and /watches/:id/edit. */
export function WatchFormPage() {
  const { id } = useParams();
  const query = useQuery({
    queryKey: ["watch", id],
    queryFn: () => getWatch(id ?? ""),
    enabled: Boolean(id),
  });
  if (!id) return <WatchForm />;
  if (query.isPending) {
    return (
      <Page title="Edit watch">
        <p className="text-muted" aria-busy>
          Loading…
        </p>
      </Page>
    );
  }
  if (query.isError) {
    if (query.error instanceof ApiError && query.error.status === 404) return <NotFound />;
    return (
      <Page title="Edit watch">
        <ErrorState
          title="The watch could not be loaded."
          error={query.error}
          onRetry={() => void query.refetch()}
        />
      </Page>
    );
  }
  return <WatchForm initial={query.data} />;
}

function Page({
  title,
  back = "/watches",
  children,
}: {
  title: string;
  back?: string;
  children: ReactNode;
}) {
  return (
    <div className="mx-auto flex w-full max-w-3xl flex-col gap-6 px-5 py-8 md:px-10 md:py-12">
      <title>{`${title} – gedour`}</title>
      <Link
        to={back}
        className="inline-flex w-fit items-center gap-1.5 text-sm text-muted hover:text-ink"
      >
        <ArrowLeft aria-hidden className="size-4" />
        Back
      </Link>
      <h1 className="font-display text-3xl font-semibold tracking-tight">{title}</h1>
      {children}
    </div>
  );
}

const sectionClass = "flex flex-col gap-4 rounded-xl border border-line bg-surface p-5 sm:p-6";

export function WatchForm({ initial }: { initial?: Watch }) {
  const editing = Boolean(initial);
  const [kind, setKind] = useState<WatchKind>(initial?.kind ?? "github_repo");
  const [values, setValues] = useState<FormValues>(initial ? fromConfig(initial.config) : {});
  const [name, setName] = useState(initial?.name ?? "");
  const [nameTouched, setNameTouched] = useState(editing);
  // null until the user picks one: the default follows the kind.
  const [scheduleChoice, setSchedule] = useState<string | null>(initial?.schedule ?? null);
  const [credentialId, setCredentialId] = useState(initial?.credential_id ?? "");
  const [paste, setPaste] = useState("");
  const [errors, setErrors] = useState<Record<string, string>>({});
  const hintId = useId();
  const toast = useToast();
  const navigate = useNavigate();
  const client = useQueryClient();
  const credentialKind = kindInfo(kind).credential;
  const schedule = scheduleChoice ?? kindInfo(kind).defaultSchedule;
  const credentials = useQuery({
    queryKey: ["credentials", credentialKind],
    queryFn: () => listCredentials(credentialKind),
  });

  const changeKind = (k: WatchKind) => {
    // A credential of another family would be refused: drop it rather than send it.
    if (kindInfo(k).credential !== credentialKind) setCredentialId("");
    setKind(k);
  };

  const recognized = paste.trim() ? recognize(paste) : undefined;
  const onPaste = (text: string) => {
    setPaste(text);
    const r = text.trim() ? recognize(text) : null;
    if (!r || editing) return;
    changeKind(r.kind);
    // The pasted target replaces every field of its kind, so nothing stale from an earlier paste stays.
    const next: FormValues = { ...values };
    for (const f of FIELDS[r.kind]) next[f.key] = f.type === "checkbox" ? false : "";
    setValues({ ...next, ...fromConfig(r.config) });
    if (!nameTouched) setName(targetSummary(r));
    setErrors({});
  };

  const save = useMutation({
    mutationFn: () => {
      const config = toConfig(kind, values);
      if (initial) {
        return updateWatch(initial.id, {
          name,
          config,
          schedule,
          credential_id: credentialId,
        });
      }
      return createWatch({
        name,
        kind,
        config,
        // Unpicked: the server applies its own per-kind default, the one shown here mirrors it.
        schedule: scheduleChoice ?? undefined,
        credential_id: credentialId || undefined,
      });
    },
    onSuccess: (w) => {
      client.setQueryData(["watch", w.id], w);
      void client.invalidateQueries({ queryKey: ["watches"] });
      void client.invalidateQueries({ queryKey: ["credentials"] });
      toast(editing ? `Saved ${w.name}` : `Created ${w.name}`);
      void navigate(`/watches/${w.id}`);
    },
    onError: (err) => {
      setErrors(fieldErrors(err));
      toast(`Could not save the watch: ${problemText(err)}`, "error");
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    setErrors({});
    save.mutate();
  };
  const setValue = (key: string, v: string | boolean) =>
    setValues((prev) => ({ ...prev, [key]: v }));
  const preview = describeSchedule(schedule);
  // Fields that render their own error; the rest go to the alert at the top.
  const shown = [
    "name",
    "schedule",
    "kind",
    "credential_id",
    ...FIELDS[kind].filter((f) => f.type !== "checkbox").map((f) => f.key),
  ];
  const topErrors = unplaced(errors, shown);

  return (
    <Page
      title={editing ? `Edit ${initial?.name}` : "New watch"}
      back={initial ? `/watches/${initial.id}` : "/watches"}
    >
      <form noValidate onSubmit={submit} className="flex flex-col gap-5" aria-label="Watch">
        {topErrors.length > 0 && (
          <div
            role="alert"
            className="rounded-lg border border-rose-500/40 bg-rose-500/8 px-4 py-3 text-sm"
          >
            {topErrors.map((m) => (
              <p key={m}>{m}</p>
            ))}
          </div>
        )}
        {!editing && (
          <section className={cn(sectionClass, "border-dashed bg-transparent")}>
            <Field
              label={
                <span className="inline-flex items-center gap-1.5">
                  <Link2 aria-hidden className="size-4" />
                  Paste a URL
                </span>
              }
              hint={
                recognized ? (
                  <span className="inline-flex items-center gap-1 text-emerald-800 dark:text-emerald-300">
                    <Sparkles aria-hidden className="size-3.5" />
                    Recognized: {kindInfo(recognized.kind).label}, fields filled in below
                  </span>
                ) : recognized === null ? (
                  "Not recognized: pick a kind and fill the fields below."
                ) : (
                  "A GitHub or GitLab repository, release, PR or MR page, a Docker Hub page or an image like ghcr.io/org/app"
                )
              }
            >
              {(p) => (
                <input
                  {...p}
                  type="text"
                  inputMode="url"
                  autoComplete="off"
                  spellCheck={false}
                  value={paste}
                  placeholder="https://github.com/golang/go/releases"
                  onChange={(e) => onPaste(e.target.value)}
                  className={inputClass}
                />
              )}
            </Field>
          </section>
        )}

        <fieldset
          className={sectionClass}
          disabled={editing}
          aria-describedby={editing ? "kind-locked" : undefined}
        >
          <legend className="sr-only">Kind</legend>
          <p className="text-sm font-medium" aria-hidden>
            Kind
          </p>
          <div className="grid gap-2 sm:grid-cols-2 lg:grid-cols-3">
            {KINDS.map((k) => (
              <label
                key={k.kind}
                className={cn(
                  "flex cursor-pointer items-start gap-3 rounded-lg border border-line bg-bg/40 p-3 transition-colors hover:border-beacon/50",
                  "has-checked:border-beacon has-checked:bg-beacon-soft has-focus-visible:ring-2 has-focus-visible:ring-beacon",
                  "has-disabled:cursor-default has-disabled:opacity-60 has-checked:has-disabled:opacity-100",
                )}
              >
                <input
                  type="radio"
                  name="kind"
                  value={k.kind}
                  checked={kind === k.kind}
                  onChange={() => changeKind(k.kind)}
                  className="sr-only"
                />
                <k.icon aria-hidden className="mt-0.5 size-5 shrink-0" />
                <span className="flex flex-col">
                  <span className="text-sm font-medium">{k.label}</span>
                  <span className="text-xs text-muted">{k.blurb}</span>
                </span>
              </label>
            ))}
          </div>
          {editing && (
            <p id="kind-locked" className="text-xs text-muted">
              The kind of a watch cannot change; create a new watch instead.
            </p>
          )}
          {errors.kind && <p className="text-sm text-rose-700 dark:text-rose-300">{errors.kind}</p>}
        </fieldset>

        <section className={sectionClass} aria-label="Target">
          <Field label="Name" error={errors.name}>
            {(p) => (
              <input
                {...p}
                value={name}
                maxLength={200}
                onChange={(e) => {
                  setName(e.target.value);
                  setNameTouched(true);
                }}
                className={inputClass}
              />
            )}
          </Field>
          {FIELDS[kind].map((f, i, all) => {
            // Boxes sharing a hint show it once, under the last of them, and all point to it.
            const last = all.findLastIndex((g) => g.hint === f.hint);
            const describedBy = f.hint ? `${hintId}-${all[last]?.key}` : undefined;
            return f.type === "checkbox" ? (
              <div key={f.key} className="flex flex-col gap-1.5">
                <label className="flex items-center gap-2 text-sm">
                  <input
                    type="checkbox"
                    checked={values[f.key] === true}
                    onChange={(e) => setValue(f.key, e.target.checked)}
                    aria-describedby={describedBy}
                    className="size-4 accent-beacon"
                  />
                  {f.label}
                </label>
                {f.hint && last === i && (
                  <p id={describedBy} className="text-xs text-muted">
                    {f.hint}
                  </p>
                )}
              </div>
            ) : (
              <Field
                key={f.key}
                label={f.optional ? `${f.label} (optional)` : f.label}
                hint={f.hint}
                error={errors[f.key]}
              >
                {(p) => (
                  <input
                    {...p}
                    type={f.type === "number" ? "number" : "text"}
                    min={f.type === "number" ? 1 : undefined}
                    spellCheck={false}
                    value={typeof values[f.key] === "string" ? (values[f.key] as string) : ""}
                    placeholder={f.placeholder}
                    onChange={(e) => setValue(f.key, e.target.value)}
                    className={cn(inputClass, f.type === "text" && "font-mono")}
                  />
                )}
              </Field>
            );
          })}
        </section>

        <section className={sectionClass} aria-label="Schedule and access">
          <Field
            label="Schedule"
            error={errors.schedule}
            hint={
              preview ??
              (schedule.trim() ? "A 5-field cron expression or a duration like 30m" : undefined)
            }
          >
            {(p) => (
              <div className="flex flex-col gap-2">
                <input
                  {...p}
                  value={schedule}
                  spellCheck={false}
                  onChange={(e) => setSchedule(e.target.value)}
                  className={cn(inputClass, "font-mono sm:max-w-64")}
                />
                <div role="group" aria-label="Schedule presets" className="flex flex-wrap gap-1.5">
                  {SCHEDULE_PRESETS.map((s) => (
                    <button
                      key={s.value}
                      type="button"
                      aria-pressed={schedule === s.value}
                      onClick={() => setSchedule(s.value)}
                      className={cn(
                        "rounded-full border border-line px-2.5 py-0.5 text-xs text-muted hover:text-ink",
                        schedule === s.value && "border-beacon bg-beacon-soft text-ink",
                      )}
                    >
                      {s.label}
                    </button>
                  ))}
                </div>
              </div>
            )}
          </Field>
          <Field
            label="Credential (optional)"
            error={errors.credential_id}
            hint={
              <>
                {credentialId && (
                  <span className="block font-medium text-ink">
                    {credentialKind === "registry_basic" ? "Login" : "Token"} is sent to{" "}
                    <span className="font-mono">{credentialHost(kind, values)}</span>
                  </span>
                )}
                Public targets need none.{" "}
                <Link
                  to="/settings/credentials"
                  className="font-medium text-ink underline underline-offset-2"
                >
                  Add a credential
                </Link>
              </>
            }
          >
            {(p) => (
              <select
                {...p}
                value={credentialId}
                onChange={(e) => setCredentialId(e.target.value)}
                className={cn(inputClass, "sm:max-w-80")}
              >
                <option value="">None</option>
                {credentials.data?.map((c) => (
                  <option key={c.id} value={c.id}>
                    {c.name}
                    {c.last4 ? ` (••••${c.last4})` : ""}
                  </option>
                ))}
              </select>
            )}
          </Field>
        </section>

        <div className="flex flex-wrap gap-2">
          <Button type="submit" variant="primary" disabled={save.isPending}>
            {save.isPending ? "Saving…" : editing ? "Save changes" : "Create watch"}
          </Button>
          <Button variant="ghost" asChild>
            <Link to={initial ? `/watches/${initial.id}` : "/watches"}>Cancel</Link>
          </Button>
        </div>
      </form>
    </Page>
  );
}

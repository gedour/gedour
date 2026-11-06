import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowLeft, KeyRound, Pencil, Trash2, UserPlus } from "lucide-react";
import { useState, type FormEvent } from "react";
import { Link, useNavigate, useParams } from "react-router";

import {
  addShare,
  ApiError,
  deleteShare,
  deleteWatch,
  getWatch,
  listCredentials,
  listShares,
  setWatchPaused,
  updateShare,
} from "@/api/client";
import { ErrorState } from "@/components/ErrorState";
import { Button } from "@/components/ui/button";
import { Dialog, DialogActions } from "@/components/ui/dialog";
import { Field, inputClass } from "@/components/ui/field";
import { ActivityTabs, StatusCard } from "@/components/WatchActivity";
import { DeleteWatchWarning, KindIcon, PausedBadge, SharedBadge } from "@/components/WatchBits";
import { useToast } from "@/hooks/toast";
import { fieldErrors, problemText } from "@/lib/problems";
import {
  describeSchedule,
  FIELDS,
  kindInfo,
  PERMISSION_LABEL,
  targetSummary,
} from "@/lib/watchKinds";
import { cn } from "@/lib/utils";
import { NotFound } from "@/pages/NotFound";
import type { SharePermission, Watch } from "@/types/api";

const sectionClass = "flex flex-col gap-4 rounded-xl border border-line bg-surface p-5 sm:p-6";

export function WatchDetailPage() {
  const { id = "" } = useParams();
  const query = useQuery({ queryKey: ["watch", id], queryFn: () => getWatch(id) });
  if (query.isError && query.error instanceof ApiError && query.error.status === 404) {
    // Not shared and not found read the same, so the existence of others' watches never leaks.
    return <NotFound />;
  }
  return (
    <div className="mx-auto flex w-full max-w-4xl flex-col gap-6 px-5 py-8 md:px-10 md:py-12">
      <title>{`${query.data?.name ?? "Watch"} – gedour`}</title>
      <Link
        to="/watches"
        className="inline-flex w-fit items-center gap-1.5 text-sm text-muted hover:text-ink"
      >
        <ArrowLeft aria-hidden className="size-4" />
        Watches
      </Link>
      {query.isError ? (
        <ErrorState
          title="The watch could not be loaded."
          error={query.error}
          onRetry={() => void query.refetch()}
        />
      ) : query.isPending ? (
        <div aria-busy className="flex flex-col gap-3" aria-label="Loading watch">
          <span aria-hidden className="h-8 w-64 animate-pulse rounded bg-line" />
          <span aria-hidden className="h-40 animate-pulse rounded-xl bg-line/60" />
        </div>
      ) : (
        <WatchDetail watch={query.data} />
      )}
    </div>
  );
}

function WatchDetail({ watch }: { watch: Watch }) {
  const canWrite = watch.permission !== "ro";
  const owner = watch.permission === "owner";
  const client = useQueryClient();
  const toast = useToast();
  const pause = useMutation({
    mutationFn: (paused: boolean) => setWatchPaused(watch.id, paused),
    onSuccess: (w) => {
      client.setQueryData(["watch", w.id], w);
      void client.invalidateQueries({ queryKey: ["watches"] });
      toast(w.paused ? `Paused ${w.name}` : `Resumed ${w.name}`);
    },
    onError: (err) => toast(`Could not change the paused state: ${problemText(err)}`, "error"),
  });
  const info = kindInfo(watch.kind);

  return (
    <>
      <header className="flex flex-wrap items-start gap-4">
        <KindIcon kind={watch.kind} className="size-12" />
        <div className="flex min-w-0 flex-1 basis-52 flex-col gap-1">
          <h1 className="font-display text-3xl font-semibold tracking-tight break-words">
            {watch.name}
          </h1>
          <p className="flex flex-wrap items-center gap-2 text-sm text-muted">
            <span>{info.label}</span>
            <span aria-hidden>·</span>
            <span className="font-mono">{targetSummary(watch)}</span>
            {watch.paused && <PausedBadge />}
            <SharedBadge watch={watch} />
          </p>
        </div>
        {canWrite && (
          <div className="flex items-center gap-2">
            <div className="flex items-center gap-2 text-sm">
              <button
                type="button"
                role="switch"
                aria-checked={watch.paused}
                aria-label="Paused"
                disabled={pause.isPending}
                onClick={() => pause.mutate(!watch.paused)}
                className={cn(
                  "relative h-6 w-11 rounded-full border border-line transition-colors disabled:opacity-60",
                  watch.paused ? "bg-beacon" : "bg-muted",
                )}
              >
                <span
                  aria-hidden
                  className={cn(
                    "absolute top-0.5 left-0.5 size-4.5 rounded-full bg-surface shadow transition-transform motion-reduce:transition-none",
                    watch.paused && "translate-x-5",
                  )}
                />
              </button>
              <span aria-hidden>Paused</span>
            </div>
            <Button variant="outline" asChild>
              <Link to={`/watches/${watch.id}/edit`}>
                <Pencil aria-hidden />
                Edit
              </Link>
            </Button>
          </div>
        )}
      </header>

      <StatusCard watch={watch} />
      <ActivityTabs watch={watch} />

      <section className={sectionClass} aria-labelledby="config-title">
        <h2 id="config-title" className="font-display text-lg font-semibold">
          Configuration
        </h2>
        <dl className="grid gap-x-6 gap-y-3 text-sm sm:grid-cols-[max-content_1fr]">
          {FIELDS[watch.kind].map((f) => {
            const v = watch.config[f.key];
            if (v === undefined || v === "") return null;
            return (
              <div key={f.key} className="contents">
                <dt className="text-muted">{f.label}</dt>
                <dd className={cn("break-all", f.type === "text" && "font-mono")}>
                  {typeof v === "boolean" ? (v ? "Yes" : "No") : String(v)}
                </dd>
              </div>
            );
          })}
          <dt className="text-muted">Schedule</dt>
          <dd>
            <span className="font-mono">{watch.schedule}</span>
            {describeSchedule(watch.schedule) && (
              <span className="ml-2 text-muted">{describeSchedule(watch.schedule)}</span>
            )}
          </dd>
          <dt className="text-muted">Credential</dt>
          <dd>
            <CredentialName watch={watch} />
          </dd>
        </dl>
      </section>

      <SharingPanel watch={watch} />
      {owner && <DangerZone watch={watch} />}
    </>
  );
}

function CredentialName({ watch }: { watch: Watch }) {
  const owner = watch.permission === "owner";
  const credentials = useQuery({
    queryKey: ["credentials", kindInfo(watch.kind).credential],
    queryFn: () => listCredentials(kindInfo(watch.kind).credential),
    enabled: owner && Boolean(watch.credential_id),
  });
  if (!watch.credential_id) return <span className="text-muted">None</span>;
  // Credentials stay private to their owner: a sharee only learns that one is attached.
  if (!owner) return <span className="text-muted">Attached by the owner</span>;
  const c = credentials.data?.find((x) => x.id === watch.credential_id);
  return (
    <span className="inline-flex items-center gap-1.5">
      <KeyRound aria-hidden className="size-3.5 text-muted" />
      {c ? c.name : "…"}
      {c?.last4 && <span className="font-mono text-muted">••••{c.last4}</span>}
    </span>
  );
}

function SharingPanel({ watch }: { watch: Watch }) {
  const owner = watch.permission === "owner";
  const client = useQueryClient();
  const toast = useToast();
  const [email, setEmail] = useState("");
  const [permission, setPermission] = useState<SharePermission>("ro");
  const [error, setError] = useState("");
  const shares = useQuery({ queryKey: ["shares", watch.id], queryFn: () => listShares(watch.id) });
  const refresh = () => void client.invalidateQueries({ queryKey: ["shares", watch.id] });
  const add = useMutation({
    mutationFn: () => addShare(watch.id, email.trim(), permission),
    onSuccess: (s) => {
      setEmail("");
      refresh();
      toast(`Shared with ${s.display_name || s.email}`);
    },
    onError: (err) => setError(fieldErrors(err).email ?? problemText(err)),
  });
  const change = useMutation({
    mutationFn: (v: { userId: string; permission: SharePermission }) =>
      updateShare(watch.id, v.userId, v.permission),
    onSuccess: refresh,
    onError: (err) => toast(`Could not change the share: ${problemText(err)}`, "error"),
  });
  const remove = useMutation({
    mutationFn: (userId: string) => deleteShare(watch.id, userId),
    onSuccess: refresh,
    onError: (err) => toast(`Could not remove the share: ${problemText(err)}`, "error"),
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    setError("");
    add.mutate();
  };

  return (
    <section className={sectionClass} aria-labelledby="sharing-title">
      <div className="flex flex-col gap-1">
        <h2 id="sharing-title" className="font-display text-lg font-semibold">
          Sharing
        </h2>
        <p className="text-sm text-muted">
          {owner
            ? "Read-only users see the watch; read-write users can also edit, pause and resume it."
            : `${watch.owner_name || "The owner"} shared this watch with you, ${PERMISSION_LABEL[watch.permission]}.`}
        </p>
      </div>
      {owner && (
        <form onSubmit={submit} className="flex flex-wrap items-start gap-2" aria-label="Share">
          <Field label="Email" error={error} className="min-w-56 flex-1">
            {(p) => (
              <input
                {...p}
                type="email"
                autoComplete="off"
                value={email}
                placeholder="someone@example.com"
                onChange={(e) => setEmail(e.target.value)}
                className={inputClass}
              />
            )}
          </Field>
          <Field label="Access">
            {(p) => (
              <select
                {...p}
                value={permission}
                onChange={(e) => setPermission(e.target.value as SharePermission)}
                className={cn(inputClass, "w-auto pr-8")}
              >
                <option value="ro">Read-only</option>
                <option value="rw">Read-write</option>
              </select>
            )}
          </Field>
          <Button
            type="submit"
            variant="primary"
            className="mt-7"
            disabled={add.isPending || !email.trim()}
          >
            <UserPlus aria-hidden />
            Share
          </Button>
        </form>
      )}
      {shares.isError ? (
        <ErrorState
          title="Shares could not be loaded."
          error={shares.error}
          onRetry={() => void shares.refetch()}
        />
      ) : shares.isPending ? (
        <span aria-hidden className="h-10 animate-pulse rounded-lg bg-line/60" />
      ) : shares.data.length === 0 ? (
        <p className="text-sm text-muted">Not shared with anyone.</p>
      ) : (
        <ul aria-label="Shared with" className="divide-y divide-line rounded-lg border border-line">
          {shares.data.map((s) => (
            <li key={s.user_id} className="flex flex-wrap items-center gap-3 px-4 py-2.5">
              <span className="flex min-w-0 flex-1 flex-col">
                <span className="truncate text-sm font-medium">{s.display_name || s.email}</span>
                <span className="truncate text-xs text-muted">{s.email}</span>
              </span>
              {owner ? (
                <>
                  <select
                    aria-label={`Access of ${s.email}`}
                    value={s.permission}
                    onChange={(e) =>
                      change.mutate({
                        userId: s.user_id,
                        permission: e.target.value as SharePermission,
                      })
                    }
                    className={cn(inputClass, "h-8 w-auto pr-8 text-xs")}
                  >
                    <option value="ro">Read-only</option>
                    <option value="rw">Read-write</option>
                  </select>
                  <Button
                    variant="ghost"
                    size="sm"
                    aria-label={`Remove ${s.email}`}
                    onClick={() => remove.mutate(s.user_id)}
                  >
                    Remove
                  </Button>
                </>
              ) : (
                <span className="text-xs text-muted">{PERMISSION_LABEL[s.permission]}</span>
              )}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

function DangerZone({ watch }: { watch: Watch }) {
  const [open, setOpen] = useState(false);
  const client = useQueryClient();
  const toast = useToast();
  const navigate = useNavigate();
  const del = useMutation({
    mutationFn: () => deleteWatch(watch.id),
    onSuccess: () => {
      client.removeQueries({ queryKey: ["watch", watch.id] });
      void client.invalidateQueries({ queryKey: ["watches"] });
      toast(`Deleted ${watch.name}`);
      void navigate("/watches");
    },
    onError: (err) => {
      setOpen(false);
      toast(`Could not delete the watch: ${problemText(err)}`, "error");
    },
  });
  return (
    <section className={cn(sectionClass, "border-rose-500/40")} aria-labelledby="danger-title">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex flex-col gap-1">
          <h2 id="danger-title" className="font-display text-lg font-semibold">
            Danger zone
          </h2>
          <p className="text-sm text-muted">
            Deleting cannot be undone. The audit history of the watch stays.
          </p>
        </div>
        <Button variant="danger" onClick={() => setOpen(true)}>
          <Trash2 aria-hidden />
          Delete watch
        </Button>
      </div>
      <Dialog
        open={open}
        title={`Delete ${watch.name}?`}
        onClose={() => setOpen(false)}
        dismissible={!del.isPending}
      >
        <DeleteWatchWarning />
        <DialogActions>
          <Button variant="ghost" disabled={del.isPending} onClick={() => setOpen(false)} autoFocus>
            Cancel
          </Button>
          <Button variant="danger" disabled={del.isPending} onClick={() => del.mutate()}>
            {del.isPending ? "Deleting…" : "Delete"}
          </Button>
        </DialogActions>
      </Dialog>
    </section>
  );
}

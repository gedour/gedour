import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Check, Copy, KeyRound, Plus, TriangleAlert } from "lucide-react";
import { useEffect, useId, useRef, useState, type FormEvent } from "react";

import { ApiError, createApiKey, listApiKeys, revokeApiKey } from "@/api/client";
import { EmptyState } from "@/components/EmptyState";
import { ErrorState } from "@/components/ErrorState";
import { Timestamp } from "@/components/Timestamp";
import { Button } from "@/components/ui/button";
import { Dialog, DialogActions } from "@/components/ui/dialog";
import { Field, inputClass } from "@/components/ui/field";
import { useToast } from "@/hooks/toast";
import { fieldErrors, problemText, unplaced } from "@/lib/problems";
import { cn } from "@/lib/utils";
import type { NavItem } from "@/nav";
import type { ApiKey, ApiKeyCreated, ApiKeyDuration, ApiKeyScope } from "@/types/api";

const MAYBE_CREATED = "The key may have been created: check the list before retrying";

const SKELETON_WIDTHS = ["w-28", "w-12", "w-20", "w-16", "w-16", "w-20", "w-14", "w-12"];
const COLUMNS = ["Name", "Scope", "Prefix", "Created", "Last used", "Effective end", "Status"];

const SCOPES: { scope: ApiKeyScope; label: string; allows: string }[] = [
  {
    scope: "read",
    label: "Read",
    allows: "Reads your inbox, watches, events, rules and deliveries.",
  },
  { scope: "write", label: "Write", allows: "Also marks inbox items and asks a watch to run." },
  {
    scope: "manage",
    label: "Manage",
    allows: "Also creates and edits watches and rules, and lists channels and credentials.",
  },
];

const DURATIONS: { value: ApiKeyDuration; label: string }[] = [
  { value: "7", label: "7 days" },
  { value: "30", label: "30 days" },
  { value: "90", label: "90 days" },
  { value: "365", label: "365 days" },
  { value: "never", label: "No end date" },
];

const STATUS_TONE: Record<ApiKey["status"], string> = {
  active: "bg-emerald-500/12 text-emerald-800 ring-emerald-600/25 dark:text-emerald-300",
  dormant: "bg-amber-500/12 text-amber-900 ring-amber-600/25 dark:text-amber-200",
  suspended: "bg-amber-500/12 text-amber-900 ring-amber-600/25 dark:text-amber-200",
  expired: "bg-line/60 text-ink ring-line",
  revoked: "bg-rose-500/12 text-rose-800 ring-rose-600/25 dark:text-rose-300",
};

export function ApiKeysPage({ item }: { item: NavItem }) {
  const query = useQuery({ queryKey: ["api-keys"], queryFn: listApiKeys });
  const [creating, setCreating] = useState(false);
  const newButton = useRef<HTMLButtonElement>(null);
  const [revokes, setRevokes] = useState(0);
  // A parent's effect runs after the dialog's: closing hands focus back to the row's Revoke button, gone at the refetch.
  useEffect(() => {
    if (revokes > 0) newButton.current?.focus();
  }, [revokes]);

  return (
    <div className="mx-auto flex w-full max-w-5xl flex-col gap-6 px-5 py-8 md:px-10 md:py-12">
      <title>{`${item.label} – gedour`}</title>
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div className="flex flex-col gap-2">
          <h1 className="font-display text-3xl font-semibold tracking-tight">{item.label}</h1>
          <p className="max-w-prose text-muted">
            A key speaks for you until it is revoked or reaches its effective end. It stops early
            when you have not signed in for a while, and works again once you do.
          </p>
        </div>
        <Button ref={newButton} variant="primary" onClick={() => setCreating(true)}>
          <Plus aria-hidden />
          New API key
        </Button>
      </div>

      {query.isError && !query.data ? (
        <ErrorState
          title="API keys could not be loaded."
          error={query.error}
          onRetry={() => void query.refetch()}
        />
      ) : !query.isPending && query.data.length === 0 ? (
        <EmptyState title={item.emptyTitle} body={item.emptyBody} />
      ) : (
        <KeyTable
          items={query.data ?? []}
          loading={query.isPending}
          onRevoked={() => setRevokes((n) => n + 1)}
        />
      )}
      <CreateDialog open={creating} onClose={() => setCreating(false)} />
    </div>
  );
}

function KeyTable({
  items,
  loading,
  onRevoked,
}: {
  items: ApiKey[];
  loading: boolean;
  onRevoked: () => void;
}) {
  const [revoking, setRevoking] = useState<ApiKey | null>(null);
  return (
    <div className="relative overflow-x-auto rounded-xl border border-line bg-surface">
      <table className="w-full min-w-[760px] text-left text-sm" aria-busy={loading}>
        <caption className="sr-only">API keys</caption>
        <thead className="border-b border-line text-xs tracking-wide text-muted uppercase">
          <tr>
            {COLUMNS.map((h) => (
              <th key={h} scope="col" className="px-4 py-3 font-medium">
                {h}
              </th>
            ))}
            <th scope="col" className="px-4 py-3">
              <span className="sr-only">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {loading
            ? Array.from({ length: 3 }, (_, i) => (
                <tr
                  key={i}
                  data-testid="api-key-skeleton"
                  className="border-b border-line last:border-0"
                >
                  {SKELETON_WIDTHS.map((w, j) => (
                    <td key={j} className="px-4 py-3.5">
                      <span
                        aria-hidden
                        className={cn("block h-3.5 animate-pulse rounded bg-line", w)}
                      />
                    </td>
                  ))}
                </tr>
              ))
            : items.map((k) => (
                <tr key={k.id} className="border-b border-line last:border-0">
                  <td className="px-4 py-3 font-medium">
                    <span className="inline-flex items-center gap-2">
                      <KeyRound aria-hidden className="size-3.5 text-muted" />
                      {k.name}
                    </span>
                  </td>
                  <td className="px-4 py-3 text-muted capitalize">{k.scope}</td>
                  <td className="px-4 py-3 font-mono text-xs">gdr_{k.prefix}…</td>
                  <td className="px-4 py-3 text-muted">
                    <Timestamp at={k.created_at} />
                  </td>
                  <td className="px-4 py-3 text-muted">
                    {k.last_used_at ? <Timestamp at={k.last_used_at} /> : "Never"}
                  </td>
                  <td className="px-4 py-3 text-muted">
                    <EffectiveEnd apiKey={k} />
                  </td>
                  <td className="px-4 py-3">
                    <span
                      className={cn(
                        "rounded-full px-2 py-0.5 text-xs font-medium capitalize ring-1 ring-inset",
                        STATUS_TONE[k.status],
                      )}
                    >
                      {k.status}
                    </span>
                  </td>
                  <td className="px-4 py-2 text-right whitespace-nowrap">
                    {k.status !== "revoked" && (
                      <Button variant="ghost" size="sm" onClick={() => setRevoking(k)}>
                        Revoke
                      </Button>
                    )}
                  </td>
                </tr>
              ))}
        </tbody>
      </table>
      {loading && <span className="sr-only">Loading API keys</span>}
      <RevokeDialog apiKey={revoking} onClose={() => setRevoking(null)} onRevoked={onRevoked} />
    </div>
  );
}

function EffectiveEnd({ apiKey }: { apiKey: ApiKey }) {
  if (apiKey.status === "revoked") return "—";
  if (apiKey.status === "dormant") return "Sign in to wake it";
  if (apiKey.status === "suspended") return "Restored at the next sign-in";
  return apiKey.effective_until ? <Timestamp at={apiKey.effective_until} /> : "No end date";
}

function CreateDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const [name, setName] = useState("");
  const [scope, setScope] = useState<ApiKeyScope>("read");
  const [duration, setDuration] = useState<ApiKeyDuration>("90");
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [created, setCreated] = useState<ApiKeyCreated | null>(null);
  const client = useQueryClient();
  const scopeId = useId();
  const create = useMutation({
    // Security: reset() in close detaches the mutation, then the cache drops it, key included, on the next tick; the default keeps it 5 minutes.
    gcTime: 0,
    mutationFn: () => createApiKey({ name, scope, duration }),
    onSuccess: setCreated,
    onError: (err) =>
      setErrors(
        err instanceof ApiError && err.status < 500 ? fieldErrors(err) : { "": MAYBE_CREATED },
      ),
    // A lost answer may hide a key the server committed: it must still reach the list, to be revoked.
    onSettled: () => void client.invalidateQueries({ queryKey: ["api-keys"] }),
  });
  const close = () => {
    setName("");
    setScope("read");
    setDuration("90");
    setErrors({});
    setCreated(null);
    create.reset();
    onClose();
  };
  const submit = (e: FormEvent) => {
    e.preventDefault();
    setErrors({});
    create.mutate();
  };
  return (
    <Dialog
      open={open}
      title={created ? `Copy ${created.name} now` : "New API key"}
      onClose={close}
      dismissible={!created && !create.isPending}
    >
      {created ? (
        <Reveal apiKey={created} onDone={close} />
      ) : (
        <form noValidate onSubmit={submit} className="flex flex-col gap-4" aria-label="API key">
          {unplaced(errors, ["name", "scope", "duration"]).map((m) => (
            <p key={m} role="alert" className="text-sm text-rose-700 dark:text-rose-300">
              {m}
            </p>
          ))}
          <Field
            label="Name"
            error={errors.name}
            hint="Where the key is used, up to 64 characters."
          >
            {(p) => (
              <input
                {...p}
                value={name}
                maxLength={64}
                autoComplete="off"
                onChange={(e) => setName(e.target.value)}
                className={inputClass}
              />
            )}
          </Field>
          <fieldset className="flex flex-col gap-2">
            <legend className="mb-1.5 text-sm font-medium">Scope</legend>
            {SCOPES.map((s) => (
              <label key={s.scope} className="flex items-start gap-2 text-sm">
                <input
                  type="radio"
                  name="scope"
                  value={s.scope}
                  checked={scope === s.scope}
                  onChange={() => setScope(s.scope)}
                  aria-labelledby={`${scopeId}-${s.scope}`}
                  aria-describedby={`${scopeId}-${s.scope}-allows`}
                  className="mt-1"
                />
                <span>
                  <span id={`${scopeId}-${s.scope}`} className="font-medium">
                    {s.label}
                  </span>
                  <span id={`${scopeId}-${s.scope}-allows`} className="block text-muted">
                    {s.allows}
                  </span>
                </span>
              </label>
            ))}
            {errors.scope && (
              <p role="alert" className="text-sm text-rose-700 dark:text-rose-300">
                {errors.scope}
              </p>
            )}
          </fieldset>
          {scope === "manage" && (
            <p className="inline-flex items-start gap-2 rounded-lg bg-amber-500/10 px-3 py-2 text-sm text-amber-900 dark:text-amber-200">
              <TriangleAlert aria-hidden className="mt-0.5 size-4 shrink-0" />A hijacked manage key
              can silence alerts by disabling a rule or pausing a watch, route events to your
              existing channels, and read your private repositories into the inbox with your
              credentials: give it only to clients that confirm writes with a person.
            </p>
          )}
          <Field
            label="Duration"
            error={errors.duration}
            hint={
              duration === "never"
                ? "The key has no end date. It still stops when you have not signed in for a while."
                : undefined
            }
          >
            {(p) => (
              <select
                {...p}
                value={duration}
                onChange={(e) => setDuration(e.target.value as ApiKeyDuration)}
                className={inputClass}
              >
                {DURATIONS.map((d) => (
                  <option key={d.value} value={d.value}>
                    {d.label}
                  </option>
                ))}
              </select>
            )}
          </Field>
          <DialogActions>
            <Button type="button" variant="ghost" disabled={create.isPending} onClick={close}>
              Cancel
            </Button>
            <Button type="submit" variant="primary" disabled={create.isPending}>
              {create.isPending ? "Creating…" : "Create key"}
            </Button>
          </DialogActions>
        </form>
      )}
    </Dialog>
  );
}

/** The one time a key is visible. */
function Reveal({ apiKey, onDone }: { apiKey: ApiKeyCreated; onDone: () => void }) {
  const snippet = `claude mcp add --scope user --transport http gedour ${new URL(document.baseURI).origin}/mcp --header "Authorization: Bearer ${apiKey.key}"`;
  return (
    <div className="flex flex-col gap-4" data-testid="api-key-reveal">
      <CopyField label="API key" value={apiKey.key} />
      <CopyField label="Client setup" value={snippet} />
      <p className="inline-flex items-start gap-2 rounded-lg bg-amber-500/10 px-3 py-2 text-sm text-amber-900 dark:text-amber-200">
        <TriangleAlert aria-hidden className="mt-0.5 size-4 shrink-0" />
        It is shown only now. If you lose it, create a new key and revoke this one.
      </p>
      <DialogActions>
        <Button variant="primary" onClick={onDone}>
          I have copied it
        </Button>
      </DialogActions>
    </div>
  );
}

function CopyField({ label, value }: { label: string; value: string }) {
  const [state, setState] = useState<"idle" | "copied" | "failed">("idle");
  const ref = useRef<HTMLInputElement>(null);
  const id = useId();
  const copy = async () => {
    try {
      // navigator.clipboard is undefined outside a secure context: the TypeError lands below too.
      await navigator.clipboard.writeText(value);
      setState("copied");
    } catch {
      ref.current?.select();
      setState("failed");
    }
  };
  return (
    <div className="flex flex-col gap-1.5">
      <label htmlFor={id} className="text-sm font-medium">
        {label}
      </label>
      <div className="flex gap-2">
        <input
          ref={ref}
          id={id}
          readOnly
          value={value}
          onFocus={(e) => e.target.select()}
          className={cn(inputClass, "min-w-0 font-mono text-xs read-only:text-ink")}
        />
        <Button
          type="button"
          variant="outline"
          aria-label={`${state === "copied" ? "Copied" : "Copy"} ${label}`}
          onClick={() => void copy()}
        >
          {state === "copied" ? <Check aria-hidden /> : <Copy aria-hidden />}
          {state === "copied" ? "Copied" : "Copy"}
        </Button>
      </div>
      {state === "failed" && (
        <p role="alert" className="text-sm text-rose-700 dark:text-rose-300">
          Could not copy. The text is selected: press Ctrl+C.
        </p>
      )}
    </div>
  );
}

function RevokeDialog({
  apiKey,
  onClose,
  onRevoked,
}: {
  apiKey: ApiKey | null;
  onClose: () => void;
  onRevoked: () => void;
}) {
  const client = useQueryClient();
  const toast = useToast();
  const revoke = useMutation({
    mutationFn: (k: ApiKey) => revokeApiKey(k.id),
    onSuccess: (_, k) => toast(`Revoked ${k.name}`),
    onError: (err) => toast(problemText(err), "error"),
    onSettled: (_, err) => {
      // A lost answer may hide a committed revoke: the list says what happened.
      void client.invalidateQueries({ queryKey: ["api-keys"] });
      if (!err) onRevoked();
      onClose();
    },
  });
  return (
    <Dialog
      open={apiKey !== null}
      title={`Revoke ${apiKey?.name ?? ""}?`}
      onClose={onClose}
      dismissible={!revoke.isPending}
    >
      {apiKey && (
        <>
          <p className="text-sm text-muted">
            Clients using this key lose access at once. This cannot be undone.
          </p>
          <DialogActions>
            <Button variant="ghost" disabled={revoke.isPending} onClick={onClose}>
              Cancel
            </Button>
            <Button
              variant="danger"
              disabled={revoke.isPending}
              onClick={() => revoke.mutate(apiKey)}
            >
              Revoke
            </Button>
          </DialogActions>
        </>
      )}
    </Dialog>
  );
}

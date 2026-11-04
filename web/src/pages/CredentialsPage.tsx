import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowLeft, KeyRound, Plus } from "lucide-react";
import { useState, type FormEvent } from "react";
import { Link } from "react-router";

import {
  ApiError,
  createCredential,
  deleteCredential,
  listCredentials,
  rotateCredentialSecret,
} from "@/api/client";
import { EmptyState } from "@/components/EmptyState";
import { ErrorState } from "@/components/ErrorState";
import { Button } from "@/components/ui/button";
import { Dialog, DialogActions } from "@/components/ui/dialog";
import { Field, inputClass } from "@/components/ui/field";
import { useToast } from "@/hooks/toast";
import { fieldErrors, problemText, unplaced } from "@/lib/problems";
import { CREDENTIAL_KINDS, credentialLabel } from "@/lib/watchKinds";
import { cn } from "@/lib/utils";
import type { Credential, CredentialKind } from "@/types/api";

const SKELETON_WIDTHS = ["w-28", "w-24", "w-16", "w-14", "w-20", "w-24"];

export function CredentialsPage() {
  const query = useQuery({ queryKey: ["credentials", "all"], queryFn: () => listCredentials() });
  const [creating, setCreating] = useState(false);

  return (
    <div className="mx-auto flex w-full max-w-5xl flex-col gap-6 px-5 py-8 md:px-10 md:py-12">
      <title>Credentials – gedour</title>
      <Link
        to="/settings"
        className="inline-flex w-fit items-center gap-1.5 text-sm text-muted hover:text-ink"
      >
        <ArrowLeft aria-hidden className="size-4" />
        Settings
      </Link>
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div className="flex flex-col gap-2">
          <h1 className="font-display text-3xl font-semibold tracking-tight">Credentials</h1>
          <p className="max-w-prose text-muted">
            Tokens and registry logins your watches use for private targets or higher rate limits.
            Stored encrypted; a secret is never shown again after you save it.
          </p>
        </div>
        <Button variant="primary" onClick={() => setCreating(true)}>
          <Plus aria-hidden />
          Add credential
        </Button>
      </div>

      {query.isError && !query.data ? (
        <ErrorState
          title="Credentials could not be loaded."
          error={query.error}
          onRetry={() => void query.refetch()}
        />
      ) : !query.isPending && query.data.length === 0 ? (
        <EmptyState
          title="No credentials yet"
          body="Public repositories and images need none. Add a token when a watch targets something private."
        />
      ) : (
        <CredentialTable items={query.data ?? []} loading={query.isPending} />
      )}
      <CreateDialog open={creating} onClose={() => setCreating(false)} />
    </div>
  );
}

function CredentialTable({ items, loading }: { items: Credential[]; loading: boolean }) {
  const [rotating, setRotating] = useState<Credential | null>(null);
  const [deleting, setDeleting] = useState<Credential | null>(null);
  return (
    <div className="relative overflow-x-auto rounded-xl border border-line bg-surface">
      <table className="w-full min-w-[640px] text-left text-sm" aria-busy={loading}>
        <caption className="sr-only">Credentials</caption>
        <thead className="border-b border-line text-xs tracking-wide text-muted uppercase">
          <tr>
            {["Name", "Kind", "Username", "Secret", "Used by"].map((h) => (
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
                  data-testid="credential-skeleton"
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
            : items.map((c) => (
                <tr key={c.id} className="border-b border-line last:border-0">
                  <td className="px-4 py-3 font-medium">
                    <span className="inline-flex items-center gap-2">
                      <KeyRound aria-hidden className="size-3.5 text-muted" />
                      {c.name}
                    </span>
                  </td>
                  <td className="px-4 py-3 text-muted">{credentialLabel(c.kind)}</td>
                  <td className="px-4 py-3">
                    {c.username ?? <span className="text-muted">—</span>}
                  </td>
                  <td
                    className="px-4 py-3 font-mono text-xs"
                    aria-label={c.last4 ? `ends in ${c.last4}` : "hidden"}
                  >
                    ••••{c.last4}
                  </td>
                  <td className="px-4 py-3 text-muted">
                    {c.used_by_count === 1 ? "1 watch" : `${c.used_by_count} watches`}
                  </td>
                  <td className="px-4 py-2 text-right whitespace-nowrap">
                    <Button variant="ghost" size="sm" onClick={() => setRotating(c)}>
                      Rotate secret
                    </Button>
                    <Button variant="ghost" size="sm" onClick={() => setDeleting(c)}>
                      Delete
                    </Button>
                  </td>
                </tr>
              ))}
        </tbody>
      </table>
      {loading && <span className="sr-only">Loading credentials</span>}
      <RotateDialog credential={rotating} onClose={() => setRotating(null)} />
      <DeleteDialog credential={deleting} onClose={() => setDeleting(null)} />
    </div>
  );
}

function SecretField({
  value,
  onChange,
  error,
  label = "Secret",
}: {
  value: string;
  onChange: (v: string) => void;
  error?: string;
  label?: string;
}) {
  return (
    <Field label={label} error={error} hint="Write-only: gedour never shows it again.">
      {(p) => (
        <input
          {...p}
          type="password"
          autoComplete="new-password"
          required
          minLength={8}
          value={value}
          onChange={(e) => onChange(e.target.value)}
          className={cn(inputClass, "font-mono")}
        />
      )}
    </Field>
  );
}

function CreateDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const [name, setName] = useState("");
  const [kind, setKind] = useState<CredentialKind>("github_token");
  const [username, setUsername] = useState("");
  const [secret, setSecret] = useState("");
  const [errors, setErrors] = useState<Record<string, string>>({});
  const client = useQueryClient();
  const toast = useToast();
  const close = () => {
    setSecret("");
    setErrors({});
    onClose();
  };
  const create = useMutation({
    mutationFn: () =>
      createCredential({
        name,
        kind,
        secret,
        username: kind === "registry_basic" ? username : undefined,
      }),
    onSuccess: (c) => {
      void client.invalidateQueries({ queryKey: ["credentials"] });
      toast(`Saved ${c.name}`);
      setName("");
      setUsername("");
      close();
    },
    onError: (err) => setErrors(fieldErrors(err)),
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    setErrors({});
    create.mutate();
  };
  return (
    <Dialog open={open} title="Add a credential" onClose={close}>
      <form noValidate onSubmit={submit} className="flex flex-col gap-4" aria-label="Credential">
        {unplaced(errors, [
          "name",
          "kind",
          "secret",
          ...(kind === "registry_basic" ? ["username"] : []),
        ]).map((m) => (
          <p key={m} role="alert" className="text-sm text-rose-700 dark:text-rose-300">
            {m}
          </p>
        ))}
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
        <Field label="Kind" error={errors.kind}>
          {(p) => (
            <select
              {...p}
              value={kind}
              onChange={(e) => setKind(e.target.value as CredentialKind)}
              className={inputClass}
            >
              {CREDENTIAL_KINDS.map((k) => (
                <option key={k.kind} value={k.kind}>
                  {k.label}
                </option>
              ))}
            </select>
          )}
        </Field>
        {kind === "registry_basic" && (
          <Field label="Username" error={errors.username}>
            {(p) => (
              <input
                {...p}
                autoComplete="off"
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                className={inputClass}
              />
            )}
          </Field>
        )}
        <SecretField
          label={kind === "registry_basic" ? "Password" : "Token"}
          value={secret}
          onChange={setSecret}
          error={errors.secret}
        />
        <DialogActions>
          <Button type="button" variant="ghost" onClick={close}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" disabled={create.isPending}>
            {create.isPending ? "Saving…" : "Save credential"}
          </Button>
        </DialogActions>
      </form>
    </Dialog>
  );
}

function RotateDialog({
  credential,
  onClose,
}: {
  credential: Credential | null;
  onClose: () => void;
}) {
  const [secret, setSecret] = useState("");
  const [error, setError] = useState("");
  const client = useQueryClient();
  const toast = useToast();
  const close = () => {
    setSecret("");
    setError("");
    onClose();
  };
  const rotate = useMutation({
    mutationFn: () => rotateCredentialSecret(credential?.id ?? "", secret),
    onSuccess: (c) => {
      void client.invalidateQueries({ queryKey: ["credentials"] });
      toast(`Rotated the secret of ${c.name}`);
      close();
    },
    onError: (err) => setError(fieldErrors(err).secret ?? problemText(err)),
  });
  return (
    <Dialog open={credential !== null} title={`Rotate ${credential?.name ?? ""}`} onClose={close}>
      <form
        noValidate
        className="flex flex-col gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          rotate.mutate();
        }}
      >
        <SecretField label="New secret" value={secret} onChange={setSecret} error={error} />
        <DialogActions>
          <Button type="button" variant="ghost" onClick={close}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" disabled={rotate.isPending}>
            Replace secret
          </Button>
        </DialogActions>
      </form>
    </Dialog>
  );
}

function DeleteDialog({
  credential,
  onClose,
}: {
  credential: Credential | null;
  onClose: () => void;
}) {
  const [inUse, setInUse] = useState<string[] | null>(null);
  const [error, setError] = useState("");
  const client = useQueryClient();
  const toast = useToast();
  const close = () => {
    setInUse(null);
    setError("");
    onClose();
  };
  const del = useMutation({
    mutationFn: () => deleteCredential(credential?.id ?? ""),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ["credentials"] });
      toast(`Deleted ${credential?.name ?? "the credential"}`);
      close();
    },
    onError: (err) => {
      // 409: still attached to watches; name them so the user knows what to detach.
      if (err instanceof ApiError && err.status === 409) {
        setInUse(err.errors.map((e) => String(e.value)));
      } else setError(problemText(err));
    },
  });
  return (
    <Dialog open={credential !== null} title={`Delete ${credential?.name ?? ""}?`} onClose={close}>
      {inUse ? (
        <div role="alert" className="flex flex-col gap-2 text-sm">
          <p>This credential is still used by:</p>
          <ul className="list-disc pl-5 font-medium">
            {inUse.map((n) => (
              <li key={n}>{n}</li>
            ))}
          </ul>
          <p className="text-muted">
            Detach it from these watches (edit each one), then delete it.
          </p>
        </div>
      ) : (
        <p className="text-sm text-muted">
          The encrypted secret is erased. Watches cannot use it any more.
        </p>
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

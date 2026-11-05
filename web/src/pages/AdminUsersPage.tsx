import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Fragment, useEffect, useId, useRef, useState } from "react";

import {
  ApiError,
  getPurgePreview,
  listAdminUsers,
  listUserWatches,
  purgeUser,
  setUserDisabled,
  transferWatch,
} from "@/api/client";
import { EmptyState } from "@/components/EmptyState";
import { ErrorState } from "@/components/ErrorState";
import { Timestamp } from "@/components/Timestamp";
import { Button } from "@/components/ui/button";
import { Dialog, DialogActions } from "@/components/ui/dialog";
import { inputClass } from "@/components/ui/field";
import { useToast } from "@/hooks/toast";
import { useMe } from "@/hooks/useMe";
import { problemText } from "@/lib/problems";
import { cn } from "@/lib/utils";
import type { NavItem } from "@/nav";
import type { AdminUser, AdminUserStatus, AdminWatch, Me } from "@/types/api";

const COLUMNS = [
  "Account",
  "Last sign-in",
  "Status",
  "Watches",
  "Channels",
  "Rules",
  "Scripts",
  "Credentials",
];
const SKELETON_WIDTHS = ["w-40", "w-20", "w-16", "w-10", "w-8", "w-8", "w-8", "w-8", "w-16"];

const STATUS: Record<AdminUserStatus, { label: string; className: string }> = {
  active: {
    label: "Active",
    className: "bg-emerald-500/10 text-emerald-800 ring-emerald-600/25 dark:text-emerald-300",
  },
  disabled: {
    label: "Disabled",
    className: "bg-amber-500/10 text-amber-800 ring-amber-600/25 dark:text-amber-300",
  },
  purged: {
    label: "Purged",
    className: "bg-line/60 text-muted ring-line",
  },
};

const accountName = (u: AdminUser) => u.display_name || u.email || "Purged account";
const plural = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`;

type Action = "toggle" | "purge";

/**
 * The id a dialog shows now. Dialogs stay mounted, so a mutation settling after Cancel acts on the
 * dialog only while it still shows the same id; otherwise it would close or fill the next one.
 */
function useOpenId(id: string | null) {
  const ref = useRef(id);
  useEffect(() => {
    ref.current = id;
  }, [id]);
  return ref;
}

export function AdminUsersPage({ item }: { item: NavItem }) {
  // Rendered inside the shell, which only shows once the session is known.
  const me = useMe().data as Me;
  const isAdmin = me.is_admin;
  const query = useQuery({
    queryKey: ["admin", "users"],
    queryFn: listAdminUsers,
    enabled: isAdmin,
  });
  const [target, setTarget] = useState<{ user: AdminUser; action: Action } | null>(null);
  const [transfer, setTransfer] = useState<{ watch: AdminWatch; owner: AdminUser } | null>(null);
  const users = query.data ?? [];
  return (
    <div className="mx-auto flex w-full max-w-6xl flex-col gap-6 px-5 py-8 md:px-10 md:py-12">
      <title>{`${item.label} – gedour`}</title>
      <div className="flex flex-col gap-2">
        <h1 className="font-display text-3xl font-semibold tracking-tight">{item.label}</h1>
        <p className="max-w-prose text-muted">
          Every account, the oldest sign-in first. Disabling one signs it out at once and stops its
          rules; its data and watches stay. Purging a disabled one deletes its data for good.
        </p>
      </div>
      {!isAdmin ? (
        <EmptyState
          title="Administrators only"
          body="This page needs the admin role. Ask an administrator if you need something changed."
        />
      ) : query.isError && !query.data ? (
        <ErrorState
          title="Accounts could not be loaded."
          error={query.error}
          onRetry={() => void query.refetch()}
        />
      ) : !query.isPending && query.data.length === 0 ? (
        <EmptyState title={item.emptyTitle} body={item.emptyBody} />
      ) : (
        <UserTable
          items={users}
          loading={query.isPending}
          self={me.id}
          onAction={(user, action) => setTarget({ user, action })}
          onTransfer={(watch, owner) => setTransfer({ watch, owner })}
        />
      )}
      <ConfirmDialog
        user={target?.action === "toggle" ? target.user : null}
        onClose={() => setTarget(null)}
      />
      <PurgeDialog
        user={target?.action === "purge" ? target.user : null}
        onClose={() => setTarget(null)}
      />
      <TransferDialog target={transfer} users={users} onClose={() => setTransfer(null)} />
    </div>
  );
}

function UserTable({
  items,
  loading,
  self,
  onAction,
  onTransfer,
}: {
  items: AdminUser[];
  loading: boolean;
  self: string;
  onAction: (u: AdminUser, action: Action) => void;
  onTransfer: (w: AdminWatch, owner: AdminUser) => void;
}) {
  return (
    <div className="relative overflow-x-auto rounded-xl border border-line bg-surface">
      <table className="w-full min-w-[860px] text-left text-sm" aria-busy={loading}>
        <caption className="sr-only">Accounts</caption>
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
                  data-testid="user-skeleton"
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
            : items.map((u) => (
                <UserRow
                  key={u.id}
                  user={u}
                  self={u.id === self}
                  onAction={(action) => onAction(u, action)}
                  onTransfer={(w) => onTransfer(w, u)}
                />
              ))}
        </tbody>
      </table>
      {loading && <span className="sr-only">Loading accounts</span>}
    </div>
  );
}

function UserRow({
  user: u,
  self,
  onAction,
  onTransfer,
}: {
  user: AdminUser;
  self: boolean;
  onAction: (action: Action) => void;
  onTransfer: (w: AdminWatch) => void;
}) {
  const s = STATUS[u.status];
  const c = u.counts;
  const name = accountName(u);
  const [open, setOpen] = useState(false);
  const panelId = useId();
  const expandable = u.status !== "purged" && c.watches > 0;
  // The admin's own row has no Disable: the API refuses it too.
  const action =
    u.status === "disabled" ? "Enable" : u.status === "active" && !self ? "Disable" : "";
  const watches = (
    <>
      {c.watches}
      {c.shared_watches > 0 && (
        <span className="text-xs text-muted"> ({c.shared_watches} shared)</span>
      )}
    </>
  );
  return (
    <Fragment>
      <tr className="border-b border-line last:border-0">
        <td className="px-4 py-3">
          <span className={cn("block font-medium", u.status === "purged" && "text-muted")}>
            {name}
            {self && <span className="ml-1.5 text-xs font-normal text-muted">(you)</span>}
          </span>
          {u.email && u.email !== name && (
            <span className="block text-xs text-muted">{u.email}</span>
          )}
        </td>
        <td className="px-4 py-3 whitespace-nowrap">
          {u.last_login_at ? (
            <Timestamp at={u.last_login_at} />
          ) : (
            <span className="text-muted">Never</span>
          )}
        </td>
        <td className="px-4 py-3">
          <span
            className={cn(
              "inline-flex items-center rounded-md px-2 py-0.5 text-xs font-medium ring-1 ring-inset",
              s.className,
            )}
          >
            {s.label}
          </span>
        </td>
        <td className="px-4 py-3 whitespace-nowrap tabular-nums">
          {expandable ? (
            <button
              type="button"
              aria-expanded={open}
              aria-controls={open ? panelId : undefined}
              aria-label={`Watches of ${name}`}
              onClick={() => setOpen(!open)}
              className="rounded underline decoration-line underline-offset-4 hover:decoration-ink"
            >
              {watches}
            </button>
          ) : (
            watches
          )}
        </td>
        {[c.channels, c.rules, c.scripts, c.credentials].map((n, i) => (
          <td key={i} className="px-4 py-3 tabular-nums">
            {n}
          </td>
        ))}
        <td className="px-4 py-2 text-right whitespace-nowrap">
          {action && (
            <Button
              variant="ghost"
              size="sm"
              aria-label={`${action} ${name}`}
              onClick={() => onAction("toggle")}
            >
              {action}
            </Button>
          )}
          {u.status === "disabled" && (
            <Button
              variant="ghost"
              size="sm"
              aria-label={`Purge ${name}`}
              onClick={() => onAction("purge")}
            >
              Purge
            </Button>
          )}
        </td>
      </tr>
      {open && expandable && (
        <tr id={panelId} className="border-b border-line bg-bg/40">
          <td colSpan={COLUMNS.length + 1} className="px-4 py-3">
            <UserWatches user={u} onTransfer={onTransfer} />
          </td>
        </tr>
      )}
    </Fragment>
  );
}

function UserWatches({
  user,
  onTransfer,
}: {
  user: AdminUser;
  onTransfer: (w: AdminWatch) => void;
}) {
  const query = useQuery({
    queryKey: ["admin", "watches", user.id],
    queryFn: () => listUserWatches(user.id),
  });
  if (query.isPending) return <p className="text-sm text-muted">Loading watches…</p>;
  if (query.isError)
    return (
      <p role="alert" className="text-sm text-rose-700 dark:text-rose-300">
        Watches could not be loaded: {problemText(query.error)}
      </p>
    );
  return (
    <ul aria-label={`Watches of ${accountName(user)}`} className="flex flex-col gap-1">
      {query.data.map((w) => (
        <li key={w.id} className="flex flex-wrap items-center gap-x-3 gap-y-1 text-sm">
          <span className="font-medium">{w.name}</span>
          <span className="text-xs text-muted">
            {[
              w.kind,
              w.credential_name && `credential ${w.credential_name}`,
              w.shares > 0 && `shared with ${plural(w.shares, "user")}`,
            ]
              .filter(Boolean)
              .join(" · ")}
          </span>
          <Button
            variant="ghost"
            size="sm"
            className="ml-auto"
            aria-label={`Transfer ${w.name}`}
            onClick={() => onTransfer(w)}
          >
            Transfer
          </Button>
        </li>
      ))}
    </ul>
  );
}

function ConfirmDialog({ user, onClose }: { user: AdminUser | null; onClose: () => void }) {
  const [error, setError] = useState("");
  const client = useQueryClient();
  const toast = useToast();
  const disable = user?.status === "active";
  const name = user ? accountName(user) : "";
  const openId = useOpenId(user?.id ?? null);
  const close = () => {
    setError("");
    onClose();
  };
  const mutation = useMutation({
    mutationFn: (u: AdminUser) => setUserDisabled(u.id, u.status === "active"),
    // From the variables: the dialog may have been closed, and user cleared, before the answer.
    onSuccess: (_, u) => {
      void client.invalidateQueries({ queryKey: ["admin", "users"] });
      void client.invalidateQueries({ queryKey: ["audit"] });
      toast(`${u.status === "active" ? "Disabled" : "Enabled"} ${accountName(u)}`);
      if (openId.current === u.id) close();
    },
    onError: (err, u) => {
      void client.invalidateQueries({ queryKey: ["admin", "users"] });
      if (openId.current !== u.id) toast(`${accountName(u)}: ${problemText(err)}`, "error");
      // A 409 means the row was stale (another admin acted): close, so the reloaded row offers the right action.
      else if (err instanceof ApiError && err.status === 409) {
        toast(problemText(err), "error");
        close();
      } else setError(problemText(err));
    },
  });
  return (
    <Dialog
      open={user !== null}
      title={`${disable ? "Disable" : "Enable"} ${name}?`}
      onClose={close}
    >
      {disable ? (
        <p className="text-sm text-muted">
          Signs the account out everywhere and refuses its sign-in. Its rules route nothing and
          queued deliveries are not sent. Its data stays, and watches it shares keep collecting for
          the others.
        </p>
      ) : (
        <p className="text-sm text-muted">
          The account can sign in again and its rules route new changes. What was skipped while
          disabled is not replayed.
        </p>
      )}
      {error && (
        <p role="alert" className="text-sm text-rose-700 dark:text-rose-300">
          {error}
        </p>
      )}
      <DialogActions>
        <Button variant="ghost" onClick={close}>
          Cancel
        </Button>
        <Button
          variant={disable ? "danger" : "primary"}
          disabled={mutation.isPending && mutation.variables.id === user?.id}
          onClick={() => mutation.mutate(user as AdminUser)}
        >
          {disable ? "Disable" : "Enable"}
        </Button>
      </DialogActions>
    </Dialog>
  );
}

function PurgeDialog({ user, onClose }: { user: AdminUser | null; onClose: () => void }) {
  const [error, setError] = useState("");
  const client = useQueryClient();
  const toast = useToast();
  const openId = useOpenId(user?.id ?? null);
  const name = user ? accountName(user) : "";
  const preview = useQuery({
    queryKey: ["admin", "purge-preview", user?.id],
    queryFn: () => getPurgePreview((user as AdminUser).id),
    enabled: user !== null,
    staleTime: 0,
  });
  const close = () => {
    setError("");
    onClose();
  };
  const mutation = useMutation({
    mutationFn: (u: AdminUser) => purgeUser(u.id),
    onSuccess: (_, u) => {
      void client.invalidateQueries({ queryKey: ["admin"] });
      void client.invalidateQueries({ queryKey: ["audit"] });
      toast(`Purged ${accountName(u)}`);
      if (openId.current === u.id) close();
    },
    onError: (err, u) => {
      void client.invalidateQueries({ queryKey: ["admin", "users"] });
      if (openId.current !== u.id) toast(`${accountName(u)}: ${problemText(err)}`, "error");
      else if (err instanceof ApiError && err.status === 409) {
        toast(problemText(err), "error");
        close();
      } else setError(problemText(err));
    },
  });
  const p = preview.data;
  const c = p?.counts;
  const shared = p?.shared_watches ?? [];
  const others = p?.other_rules ?? [];
  return (
    <Dialog open={user !== null} title={`Purge ${name}?`} onClose={close}>
      <p className="text-sm text-muted">
        Deletes the account&apos;s data for good and blanks its email and name. The audit log stays,
        and the account stays refused at sign-in. This cannot be undone.
      </p>
      {preview.isPending ? (
        <p className="text-sm text-muted">Loading what will be deleted…</p>
      ) : preview.isError ? (
        <p role="alert" className="text-sm text-rose-700 dark:text-rose-300">
          {problemText(preview.error)}
        </p>
      ) : (
        c && (
          <>
            <p className="text-sm" data-testid="purge-counts">
              {[
                plural(c.watches, "watch", "watches"),
                plural(c.channels, "channel"),
                plural(c.rules, "rule"),
                plural(c.scripts, "script"),
                plural(c.credential_count, "credential"),
                plural(c.shares_received, "share") + " received",
                plural(c.inbox_items, "inbox item"),
              ].join(", ")}
              .
            </p>
            {(shared.length > 0 || others.length > 0) && (
              <div className="flex flex-col gap-2 text-sm">
                <p className="font-medium">Other users lose these:</p>
                <ul aria-label="Shared watches deleted" className="list-disc pl-5">
                  {shared.map((w) => (
                    <li key={w.id}>
                      Watch {w.name}, shared with {(w.shared_with as string[]).join(", ")}
                    </li>
                  ))}
                  {others.map((r) => (
                    <li key={r.id}>
                      Rule {r.name} of {r.owner}
                    </li>
                  ))}
                </ul>
                <p className="text-muted">Transfer a watch first to keep it for them.</p>
              </div>
            )}
          </>
        )
      )}
      {error && (
        <p role="alert" className="text-sm text-rose-700 dark:text-rose-300">
          {error}
        </p>
      )}
      <DialogActions>
        <Button variant="ghost" onClick={close}>
          Cancel
        </Button>
        <Button
          variant="danger"
          disabled={
            !p || preview.isFetching || (mutation.isPending && mutation.variables.id === user?.id)
          }
          onClick={() => mutation.mutate(user as AdminUser)}
        >
          Purge
        </Button>
      </DialogActions>
    </Dialog>
  );
}

function TransferDialog({
  target,
  users,
  onClose,
}: {
  target: { watch: AdminWatch; owner: AdminUser } | null;
  users: AdminUser[];
  onClose: () => void;
}) {
  const [error, setError] = useState("");
  const [to, setTo] = useState("");
  const client = useQueryClient();
  const toast = useToast();
  const selectId = useId();
  const w = target?.watch;
  const openId = useOpenId(w?.id ?? null);
  const candidates = users.filter((u) => u.status === "active" && u.id !== target?.owner.id);
  const close = () => {
    setError("");
    setTo("");
    onClose();
  };
  const mutation = useMutation({
    mutationFn: (v: { watch: AdminWatch; to: AdminUser }) => transferWatch(v.watch.id, v.to.id),
    onSuccess: (_, v) => {
      void client.invalidateQueries({ queryKey: ["admin"] });
      void client.invalidateQueries({ queryKey: ["audit"] });
      toast(`Transferred ${v.watch.name} to ${accountName(v.to)}`);
      if (openId.current === v.watch.id) close();
    },
    onError: (err, v) => {
      // A 409 can mean the watch or the accounts changed: reload what the dialog was built from.
      void client.invalidateQueries({ queryKey: ["admin"] });
      if (openId.current !== v.watch.id) toast(`${v.watch.name}: ${problemText(err)}`, "error");
      else setError(problemText(err));
    },
  });
  const chosen = candidates.find((u) => u.id === to);
  return (
    <Dialog open={target !== null} title={`Transfer ${w?.name ?? ""}?`} onClose={close}>
      <div className="flex flex-col gap-1.5">
        <label htmlFor={selectId} className="text-sm font-medium">
          New owner
        </label>
        <select
          id={selectId}
          value={to}
          onChange={(e) => setTo(e.target.value)}
          className={cn(inputClass, "pr-8")}
        >
          <option value="">Choose an active account</option>
          {candidates.map((u) => (
            <option key={u.id} value={u.id}>
              {accountName(u)}
              {u.email && u.email !== accountName(u) ? ` (${u.email})` : ""}
            </option>
          ))}
        </select>
      </div>
      {w && target && (
        <ul className="list-disc pl-5 text-sm text-muted">
          {w.credential_name && (
            <li>
              Its credential {w.credential_name} moves with it, unless another watch of{" "}
              {accountName(target.owner)} uses it.
            </li>
          )}
          <li>A share the new owner held on it is dropped; other users keep theirs.</li>
        </ul>
      )}
      {error && (
        <p role="alert" className="text-sm text-rose-700 dark:text-rose-300">
          {error}
        </p>
      )}
      <DialogActions>
        <Button variant="ghost" onClick={close}>
          Cancel
        </Button>
        <Button
          variant="primary"
          disabled={!chosen || (mutation.isPending && mutation.variables.watch.id === w?.id)}
          onClick={() => mutation.mutate({ watch: w as AdminWatch, to: chosen as AdminUser })}
        >
          Transfer
        </Button>
      </DialogActions>
    </Dialog>
  );
}

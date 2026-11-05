import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { ApiError, listAdminUsers, setUserDisabled } from "@/api/client";
import { EmptyState } from "@/components/EmptyState";
import { ErrorState } from "@/components/ErrorState";
import { Timestamp } from "@/components/Timestamp";
import { Button } from "@/components/ui/button";
import { Dialog, DialogActions } from "@/components/ui/dialog";
import { useToast } from "@/hooks/toast";
import { useMe } from "@/hooks/useMe";
import { problemText } from "@/lib/problems";
import { cn } from "@/lib/utils";
import type { NavItem } from "@/nav";
import type { AdminUser, AdminUserStatus, Me } from "@/types/api";

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

export function AdminUsersPage({ item }: { item: NavItem }) {
  // Rendered inside the shell, which only shows once the session is known.
  const me = useMe().data as Me;
  const isAdmin = me.is_admin;
  const query = useQuery({
    queryKey: ["admin", "users"],
    queryFn: listAdminUsers,
    enabled: isAdmin,
  });
  const [target, setTarget] = useState<AdminUser | null>(null);
  return (
    <div className="mx-auto flex w-full max-w-6xl flex-col gap-6 px-5 py-8 md:px-10 md:py-12">
      <title>{`${item.label} – gedour`}</title>
      <div className="flex flex-col gap-2">
        <h1 className="font-display text-3xl font-semibold tracking-tight">{item.label}</h1>
        <p className="max-w-prose text-muted">
          Every account, the oldest sign-in first. Disabling one signs it out at once and stops its
          rules; its data and watches stay.
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
          items={query.data ?? []}
          loading={query.isPending}
          self={me.id}
          onToggle={setTarget}
        />
      )}
      <ConfirmDialog user={target} onClose={() => setTarget(null)} />
    </div>
  );
}

function UserTable({
  items,
  loading,
  self,
  onToggle,
}: {
  items: AdminUser[];
  loading: boolean;
  self: string;
  onToggle: (u: AdminUser) => void;
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
                <UserRow key={u.id} user={u} self={u.id === self} onToggle={() => onToggle(u)} />
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
  onToggle,
}: {
  user: AdminUser;
  self: boolean;
  onToggle: () => void;
}) {
  const s = STATUS[u.status];
  const c = u.counts;
  const name = accountName(u);
  // The admin's own row has no Disable: the API refuses it too.
  const action =
    u.status === "disabled" ? "Enable" : u.status === "active" && !self ? "Disable" : "";
  return (
    <tr className="border-b border-line last:border-0">
      <td className="px-4 py-3">
        <span className={cn("block font-medium", u.status === "purged" && "text-muted")}>
          {name}
          {self && <span className="ml-1.5 text-xs font-normal text-muted">(you)</span>}
        </span>
        {u.email && u.email !== name && <span className="block text-xs text-muted">{u.email}</span>}
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
        {c.watches}
        {c.shared_watches > 0 && (
          <span className="text-xs text-muted"> ({c.shared_watches} shared)</span>
        )}
      </td>
      {[c.channels, c.rules, c.scripts, c.credentials].map((n, i) => (
        <td key={i} className="px-4 py-3 tabular-nums">
          {n}
        </td>
      ))}
      <td className="px-4 py-2 text-right whitespace-nowrap">
        {action && (
          <Button variant="ghost" size="sm" aria-label={`${action} ${name}`} onClick={onToggle}>
            {action}
          </Button>
        )}
      </td>
    </tr>
  );
}

function ConfirmDialog({ user, onClose }: { user: AdminUser | null; onClose: () => void }) {
  const [error, setError] = useState("");
  const client = useQueryClient();
  const toast = useToast();
  const disable = user?.status === "active";
  const name = user ? accountName(user) : "";
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
      close();
    },
    onError: (err) => {
      void client.invalidateQueries({ queryKey: ["admin", "users"] });
      // A 409 means the row was stale (another admin acted): close, so the reloaded row offers the right action.
      if (err instanceof ApiError && err.status === 409) {
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
          disabled={mutation.isPending}
          onClick={() => mutation.mutate(user as AdminUser)}
        >
          {disable ? "Disable" : "Enable"}
        </Button>
      </DialogActions>
    </Dialog>
  );
}

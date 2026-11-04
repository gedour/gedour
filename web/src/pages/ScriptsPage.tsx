import { useQuery } from "@tanstack/react-query";
import { FileCode2, Plus } from "lucide-react";
import { Link } from "react-router";

import { listScripts } from "@/api/client";
import { EmptyState } from "@/components/EmptyState";
import { ErrorState } from "@/components/ErrorState";
import { Button } from "@/components/ui/button";
import { formatRelative } from "@/lib/time";
import type { NavItem } from "@/nav";

export function NewScriptButton() {
  return (
    <Button variant="primary" asChild>
      <Link to="/scripts/new">
        <Plus aria-hidden />
        New script
      </Link>
    </Button>
  );
}

function usedBy(n: number) {
  if (n === 0) return "No channel runs it";
  return `Used by ${n} channel${n === 1 ? "" : "s"}`;
}

export function ScriptsPage({ item }: { item: NavItem }) {
  const query = useQuery({ queryKey: ["scripts"], queryFn: listScripts });
  return (
    <div className="mx-auto flex w-full max-w-6xl flex-col gap-6 px-5 py-8 md:px-10 md:py-12">
      <title>{`${item.label} – gedour`}</title>
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div className="flex flex-col gap-2">
          <h1 className="font-display text-3xl font-semibold tracking-tight">{item.label}</h1>
          <p className="max-w-prose text-muted">
            JavaScript run in a sandbox for each event a rule sends to a script channel: 5 seconds,
            no filesystem, and <code className="font-mono text-sm">http.fetch</code> to public
            addresses only.
          </p>
        </div>
        <NewScriptButton />
      </div>
      {query.isError && !query.data ? (
        <ErrorState
          title="Scripts could not be loaded."
          error={query.error}
          onRetry={() => void query.refetch()}
        />
      ) : query.isPending ? (
        <ul aria-busy aria-label="Loading scripts" className="flex flex-col gap-2">
          {Array.from({ length: 3 }, (_, i) => (
            <li
              key={i}
              data-testid="script-skeleton"
              className="flex flex-col gap-2 rounded-xl border border-line bg-surface px-4 py-4"
            >
              <span aria-hidden className="h-3.5 w-40 animate-pulse rounded bg-line" />
              <span aria-hidden className="h-3 w-64 animate-pulse rounded bg-line" />
            </li>
          ))}
        </ul>
      ) : query.data.length === 0 ? (
        <EmptyState title={item.emptyTitle} body={item.emptyBody} action={<NewScriptButton />} />
      ) : (
        <ul aria-label="Scripts" className="flex flex-col gap-2">
          {query.data.map((s) => {
            const channels = s.channels ?? [];
            return (
              <li key={s.id}>
                <Link
                  to={`/scripts/${s.id}`}
                  className="flex items-center gap-4 rounded-xl border border-line bg-surface px-4 py-3.5 transition-colors hover:border-beacon/60"
                >
                  <span className="grid size-10 shrink-0 place-items-center rounded-lg bg-beacon-soft text-ink ring-1 ring-beacon/25 ring-inset">
                    <FileCode2 aria-hidden className="size-5" />
                  </span>
                  <span className="flex min-w-0 flex-1 flex-col gap-0.5">
                    <span className="truncate font-medium">{s.name}</span>
                    <span
                      className="truncate text-xs text-muted"
                      title={channels.map((c) => c.name).join(", ") || undefined}
                    >
                      {usedBy(channels.length)} · {(s.size / 1024).toFixed(1)} KiB
                    </span>
                  </span>
                  <span className="hidden text-xs whitespace-nowrap text-muted sm:block">
                    Updated {formatRelative(new Date(s.updated_at))}
                  </span>
                </Link>
              </li>
            );
          })}
        </ul>
      )}
    </div>
  );
}

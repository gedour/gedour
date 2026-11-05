import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Inbox, Plus } from "lucide-react";
import { Link } from "react-router";

import { listChannels, listRules, updateRule } from "@/api/client";
import { ChannelIcon } from "@/components/ChannelBits";
import { EmptyState } from "@/components/EmptyState";
import { ErrorState } from "@/components/ErrorState";
import { PriorityBadge } from "@/components/PriorityBadge";
import { Button } from "@/components/ui/button";
import { useToast } from "@/hooks/toast";
import { problemText } from "@/lib/problems";
import { cn } from "@/lib/utils";
import type { NavItem } from "@/nav";
import type { Channel, Rule } from "@/types/api";

export function NewRuleButton() {
  return (
    <Button variant="primary" asChild>
      <Link to="/rules/new">
        <Plus aria-hidden />
        New rule
      </Link>
    </Button>
  );
}

const chip =
  "inline-flex items-center gap-1 rounded-md bg-bg px-2 py-0.5 text-xs font-medium text-ink ring-1 ring-line ring-inset";

export function RulesPage({ item }: { item: NavItem }) {
  const rules = useQuery({ queryKey: ["rules"], queryFn: listRules });
  const channels = useQuery({ queryKey: ["channels"], queryFn: listChannels });
  const byId = new Map((channels.data ?? []).map((c) => [c.id, c]));
  return (
    <div className="mx-auto flex w-full max-w-6xl flex-col gap-6 px-5 py-8 md:px-10 md:py-12">
      <title>{`${item.label} – gedour`}</title>
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div className="flex flex-col gap-2">
          <h1 className="font-display text-3xl font-semibold tracking-tight">{item.label}</h1>
          <p className="max-w-prose text-muted">
            A rule lets the changes of a watch (or of all of them) through a filter, into your inbox
            and the channels you pick.
          </p>
        </div>
        <NewRuleButton />
      </div>
      {rules.isError && !rules.data ? (
        <ErrorState
          title="Rules could not be loaded."
          error={rules.error}
          onRetry={() => void rules.refetch()}
        />
      ) : rules.isPending ? (
        <ul aria-busy aria-label="Loading rules" className="flex flex-col gap-2">
          {Array.from({ length: 3 }, (_, i) => (
            <li
              key={i}
              data-testid="rule-skeleton"
              className="flex flex-col gap-2 rounded-xl border border-line bg-surface px-4 py-4"
            >
              <span aria-hidden className="h-3.5 w-48 animate-pulse rounded bg-line" />
              <span aria-hidden className="h-3 w-72 animate-pulse rounded bg-line" />
            </li>
          ))}
        </ul>
      ) : rules.data.length === 0 ? (
        <EmptyState title={item.emptyTitle} body={item.emptyBody} action={<NewRuleButton />} />
      ) : (
        <ul aria-label="Rules" className="flex flex-col gap-2">
          {rules.data.map((r) => (
            <RuleRow key={r.id} rule={r} channels={byId} />
          ))}
        </ul>
      )}
    </div>
  );
}

function RuleRow({ rule, channels }: { rule: Rule; channels: Map<string, Channel> }) {
  const client = useQueryClient();
  const toast = useToast();
  const toggle = useMutation({
    mutationFn: () => updateRule(rule.id, { enabled: !rule.enabled }),
    onSuccess: (r) => {
      void client.invalidateQueries({ queryKey: ["rules"] });
      toast(`${r.name} ${r.enabled ? "enabled" : "disabled"}`);
    },
    onError: (err) => toast(`Could not change the rule: ${problemText(err)}`, "error"),
  });
  return (
    <li
      className={cn(
        "flex flex-wrap items-center gap-4 rounded-xl border border-line bg-surface px-4 py-3.5",
        !rule.enabled && "opacity-70",
      )}
    >
      <div className="flex min-w-0 flex-1 flex-col gap-1.5">
        <Link
          to={`/rules/${rule.id}`}
          className="w-fit truncate font-medium underline-offset-4 hover:underline"
        >
          {rule.name}
        </Link>
        {rule.priority === "high" && <PriorityBadge />}
        <p className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted">
          <span>
            {rule.watch_id ? (rule.watch_name ?? "A watch you can no longer read") : "All watches"}
          </span>
          <span aria-hidden>·</span>
          <code className="max-w-md truncate rounded bg-bg px-1.5 py-px font-mono text-ink">
            {rule.filter || "every event"}
          </code>
        </p>
        <ul aria-label={`Channels of ${rule.name}`} className="flex flex-wrap gap-1.5">
          <li className={chip}>
            <Inbox aria-hidden className="size-3" />
            Inbox
          </li>
          {(rule.channel_ids ?? []).map((id) => {
            const c = channels.get(id);
            return (
              <li key={id} className={chip}>
                {c && <ChannelIcon type={c.type} className="size-3" />}
                {c?.name ?? "…"}
              </li>
            );
          })}
        </ul>
      </div>
      <button
        type="button"
        role="switch"
        aria-checked={rule.enabled}
        aria-label={`${rule.name} enabled`}
        disabled={toggle.isPending}
        onClick={() => toggle.mutate()}
        className={cn(
          "relative h-6 w-11 shrink-0 rounded-full transition-colors disabled:opacity-50",
          rule.enabled ? "bg-beacon" : "bg-muted",
        )}
      >
        <span
          aria-hidden
          className={cn(
            "absolute top-0.5 left-0.5 size-5 rounded-full bg-surface shadow transition-transform motion-reduce:transition-none",
            rule.enabled && "translate-x-5",
          )}
        />
      </button>
    </li>
  );
}

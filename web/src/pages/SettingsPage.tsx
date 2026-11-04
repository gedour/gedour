import { ChevronRight, KeyRound } from "lucide-react";
import { Link } from "react-router";

import type { NavItem } from "@/nav";

export function SettingsPage({ item }: { item: NavItem }) {
  return (
    <div className="mx-auto flex w-full max-w-5xl flex-col gap-8 px-5 py-8 md:px-10 md:py-12">
      <title>{`${item.label} – gedour`}</title>
      <h1 className="font-display text-3xl font-semibold tracking-tight">{item.label}</h1>
      <ul className="grid gap-3 sm:grid-cols-2">
        <li>
          <Link
            to="/settings/credentials"
            className="flex items-center gap-4 rounded-xl border border-line bg-surface px-5 py-4 transition-colors hover:border-beacon/50 hover:bg-beacon-soft/30"
          >
            <KeyRound aria-hidden className="size-5 text-muted" />
            <span className="flex flex-1 flex-col">
              <span className="font-medium">Credentials</span>
              <span className="text-sm text-muted">
                Tokens and registry logins for your watches
              </span>
            </span>
            <ChevronRight aria-hidden className="size-4 text-muted" />
          </Link>
        </li>
      </ul>
    </div>
  );
}

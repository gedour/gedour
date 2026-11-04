import { LogOut } from "lucide-react";
import { useState } from "react";

import { signOut as endSession } from "@/api/client";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { useMe } from "@/hooks/useMe";
import { initials } from "@/lib/utils";
import type { Me } from "@/types/api";

export function UserMenu() {
  // AuthGate renders the shell only once the user is loaded.
  const me = useMe().data as Me;
  const [error, setError] = useState(false);

  const name = me.display_name || me.email || "Signed in";
  const signOut = async () => {
    setError(false);
    try {
      // The provider ends its own session too, then returns to the signed-out page.
      await endSession();
    } catch {
      setError(true);
    }
  };

  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="harbor" className="gap-2 px-1.5 sm:pr-3">
          <span
            aria-hidden
            className="grid size-7 place-items-center rounded-full bg-harbor-beacon text-xs font-semibold text-harbor"
          >
            {initials(name)}
          </span>
          {/* No aria-label (WCAG 2.5.3); one text node, as jsdom trims spaces at element edges. */}
          <span className="sr-only">{`Account: ${name}`}</span>
          <span aria-hidden className="hidden max-w-40 truncate sm:inline">
            {name}
          </span>
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="w-64">
        <DropdownMenuLabel>
          <span className="block truncate font-medium">{name}</span>
          {me.email && me.email !== name && (
            <span className="block truncate text-xs text-muted">{me.email}</span>
          )}
        </DropdownMenuLabel>
        <DropdownMenuSeparator />
        {me.dev_mode ? (
          <p className="px-2 py-1.5 text-xs text-muted">Dev mode: single user, no sign-out.</p>
        ) : (
          <DropdownMenuItem
            onSelect={(e) => {
              // Stay open so a failure can be shown in place.
              e.preventDefault();
              void signOut();
            }}
          >
            <LogOut aria-hidden />
            Sign out
          </DropdownMenuItem>
        )}
        {error && (
          <p role="alert" className="px-2 py-1.5 text-xs text-red-600 dark:text-red-400">
            Sign-out failed, try again.
          </p>
        )}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

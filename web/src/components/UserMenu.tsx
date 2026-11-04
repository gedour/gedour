import { LogOut, UserRound } from "lucide-react";
import { useState } from "react";

import { logout, navigation } from "@/api/client";
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

export function UserMenu() {
  const { data: me, isPending, isError } = useMe();
  const [error, setError] = useState(false);

  if (isPending) {
    return <span aria-hidden className="size-8 animate-pulse rounded-full bg-harbor-hover" />;
  }
  if (isError) {
    return (
      <Button variant="harbor" asChild>
        <a href={`/auth/login?return_to=${encodeURIComponent(window.location.pathname)}`}>
          <UserRound aria-hidden />
          Sign in
        </a>
      </Button>
    );
  }

  const name = me.display_name || me.email || "Signed in";
  const signOut = async () => {
    setError(false);
    try {
      // The provider ends its own session too, then returns to the signed-out page.
      navigation.go(await logout());
    } catch {
      setError(true);
    }
  };

  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="harbor" className="gap-2 px-1.5 sm:pr-3" aria-label={`Account: ${name}`}>
          <span
            aria-hidden
            className="grid size-7 place-items-center rounded-full bg-harbor-beacon text-xs font-semibold text-harbor"
          >
            {initials(name)}
          </span>
          <span className="hidden max-w-40 truncate sm:inline">{name}</span>
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

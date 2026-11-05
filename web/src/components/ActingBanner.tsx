import { useMutation, useQueryClient } from "@tanstack/react-query";
import { UserRoundCog } from "lucide-react";
import { useNavigate } from "react-router";

import { stopActing } from "@/api/client";
import { Button } from "@/components/ui/button";
import { useToast } from "@/hooks/toast";
import { useMe } from "@/hooks/useMe";
import { problemText } from "@/lib/problems";
import type { Me } from "@/types/api";

/** Shown on every page while the session acts as another user (006-AC15). */
export function ActingBanner() {
  const me = useMe().data as Me;
  const client = useQueryClient();
  const toast = useToast();
  const navigate = useNavigate();
  const stop = useMutation({
    mutationFn: stopActing,
    // Every cached query holds the other user's data, and the page shown may be one of theirs; a 409
    // (another tab stopped, or a purge did) leaves the session back to the admin all the same.
    onSettled: () => {
      void client.invalidateQueries();
      void navigate("/inbox");
    },
    onError: (err) => toast(problemText(err), "error"),
  });
  if (!me.acting_as) return null;
  const name = me.display_name || me.email;
  return (
    <div
      role="region"
      aria-label="Acting as another user"
      className="flex shrink-0 items-center gap-3 bg-amber-300 px-4 py-1.5 text-sm text-amber-950"
    >
      <UserRoundCog aria-hidden className="size-4 shrink-0" />
      <p className="min-w-0 truncate">
        Acting as <strong className="font-semibold">{name}</strong>
      </p>
      <Button
        size="sm"
        variant="primary"
        className="ml-auto"
        disabled={stop.isPending}
        onClick={() => stop.mutate()}
      >
        Stop
      </Button>
    </div>
  );
}

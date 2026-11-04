import { useSyncExternalStore } from "react";

import { auth } from "@/api/client";
import { AppShell } from "@/components/AppShell";
import { useMe } from "@/hooks/useMe";
import { SignInGate } from "@/pages/SignInGate";

/** Renders the shell only once the session is confirmed; the sign-in gate whenever it is gone. */
export function AuthGate() {
  const signedOut = useSyncExternalStore(auth.subscribe, auth.signedOut);
  const me = useMe();
  if (signedOut) return <SignInGate focusHeading={me.data !== undefined} />;
  // Data first: a failed background recheck (server restarting) must not hide a live session.
  if (me.data) return <AppShell />;
  if (me.isPending) {
    return (
      <div className="min-h-dvh bg-harbor">
        <p role="status" className="sr-only">
          Loading
        </p>
      </div>
    );
  }
  return <SignInGate onRetry={() => void me.refetch()} />;
}

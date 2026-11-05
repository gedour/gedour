import { LogIn, RotateCw } from "lucide-react";
import { useEffect, useRef } from "react";
import { useLocation } from "react-router";

import { Wordmark } from "@/components/Wordmark";

// Keyed by the server's fixed `?auth=` values: the query only picks a text, it is never rendered.
const REASONS: Record<string, { title: string; body: string }> = {
  unavailable: {
    title: "Sign-in service unavailable",
    body: "The identity provider cannot be reached right now.",
  },
  failed: {
    title: "Sign-in failed, try again",
    body: "The sign-in did not complete. Start it again from here.",
  },
  forbidden: {
    title: "No access to gedour",
    body: "Your account is not allowed to use gedour. Ask an administrator for access.",
  },
};

const ACTION =
  "mt-3 inline-flex h-10 items-center gap-2 rounded-md bg-harbor px-4 text-sm font-medium text-white transition-colors hover:bg-harbor-hover [&_svg]:size-4";

/** Where "Sign in" goes: the current URL, without the gate's own reason, is the return target. */
function loginHref(pathname: string, search: string, hash: string): string {
  // Raw pairs, not URLSearchParams: re-encoding would rewrite what the user asked for (";", "%zz").
  // Each key is decoded as the reason reader decodes it, so "%61uth" goes too.
  const pairs = search
    .slice(1)
    .split("&")
    // The "?" prefix keeps a pair's own leading "?" in its key, as the reader parses it ("?auth" stays).
    .filter((p) => p !== "" && !new URLSearchParams(`?${p}`).has("auth"));
  const target = pathname + (pairs.length ? `?${pairs.join("&")}` : "") + hash;
  return `/auth/login?return_to=${encodeURIComponent(target)}`;
}

/**
 * Full-screen gate shown in place of the app without a session (002-AC1). With onRetry it says the
 * server itself did not answer the session check, and retries that check instead of signing in.
 */
export function SignInGate({
  onRetry,
  focusHeading = false,
}: {
  onRetry?: () => void;
  /** The gate replaced a live app: move focus off the controls that just vanished. */
  focusHeading?: boolean;
}) {
  const { pathname, search, hash } = useLocation();
  const heading = useRef<HTMLHeadingElement>(null);
  useEffect(() => {
    if (focusHeading) heading.current?.focus();
  }, [focusHeading]);
  const key = new URLSearchParams(search).get("auth") ?? "";
  // Own keys only: "constructor" or "__proto__" must not reach Object.prototype.
  const reason = Object.hasOwn(REASONS, key) ? REASONS[key] : undefined;
  let title = reason?.title ?? "Sign in to continue";
  let body = reason?.body ?? "Your session is not active on this device.";
  if (onRetry) {
    title = "gedour is unreachable";
    body = "The server did not answer. Check your connection, then try again.";
  }
  return (
    <main className="grid min-h-dvh place-items-center bg-harbor px-5">
      <title>Sign in – gedour</title>
      <div className="flex w-full max-w-sm flex-col items-start gap-6">
        <Wordmark />
        <section
          aria-labelledby="sign-in-title"
          className="flex w-full flex-col items-start gap-3 rounded-2xl bg-surface px-7 py-8 text-ink shadow-2xl shadow-black/30"
        >
          <h1
            id="sign-in-title"
            ref={heading}
            tabIndex={-1}
            className="font-display text-2xl font-semibold tracking-tight outline-none"
          >
            {title}
          </h1>
          <p className="text-muted">{body}</p>
          {onRetry ? (
            <button type="button" className={ACTION} onClick={onRetry}>
              <RotateCw aria-hidden />
              Try again
            </button>
          ) : (
            <a href={loginHref(pathname, search, hash)} className={ACTION}>
              <LogIn aria-hidden />
              {reason ? "Try again" : "Sign in"}
            </a>
          )}
        </section>
      </div>
    </main>
  );
}

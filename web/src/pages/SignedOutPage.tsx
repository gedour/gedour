import { LogIn } from "lucide-react";

import { Wordmark } from "@/components/Wordmark";

// Outside the shell on purpose: nothing here calls the API, so a reload stays signed out.
export function SignedOutPage() {
  return (
    <main className="grid min-h-dvh place-items-center bg-harbor px-5">
      <title>Signed out – gedour</title>
      <div className="flex w-full max-w-sm flex-col items-start gap-6">
        <Wordmark />
        <section
          aria-labelledby="signed-out-title"
          className="flex w-full flex-col items-start gap-3 rounded-2xl bg-surface px-7 py-8 text-ink shadow-2xl shadow-black/30"
        >
          <h1 id="signed-out-title" className="font-display text-2xl font-semibold tracking-tight">
            You are signed out
          </h1>
          <p className="text-muted">Your session has ended on this device.</p>
          <a
            href="/auth/login?return_to=%2F"
            className="mt-3 inline-flex h-10 items-center gap-2 rounded-md bg-harbor px-4 text-sm font-medium text-white transition-colors hover:bg-harbor-hover [&_svg]:size-4"
          >
            <LogIn aria-hidden />
            Sign in again
          </a>
        </section>
      </div>
    </main>
  );
}

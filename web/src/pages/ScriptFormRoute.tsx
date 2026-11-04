import { lazy, Suspense } from "react";

// The editor pulls in CodeMirror: its own chunk, loaded on the first visit.
const ScriptFormPage = lazy(async () => ({
  default: (await import("@/pages/ScriptFormPage")).ScriptFormPage,
}));

export function ScriptFormRoute() {
  return (
    <Suspense
      fallback={
        <p className="px-5 py-8 text-muted md:px-10 md:py-12" aria-busy>
          Loading…
        </p>
      }
    >
      <ScriptFormPage />
    </Suspense>
  );
}

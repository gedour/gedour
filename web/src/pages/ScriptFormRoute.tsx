import { lazy, Suspense } from "react";

import { chunkLoaded, ChunkLoadError } from "@/lib/chunks";

// The editor pulls in CodeMirror: its own chunk, loaded on the first visit.
const ScriptFormPage = lazy(async () => {
  try {
    const { ScriptFormPage } = await import("@/pages/ScriptFormPage");
    chunkLoaded();
    return { default: ScriptFormPage };
  } catch (cause) {
    throw new ChunkLoadError(cause);
  }
});

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

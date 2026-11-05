import { useEffect } from "react";
import { useRouteError } from "react-router";

import { reloadPage } from "@/api/client";
import { ErrorState } from "@/components/ErrorState";
import { ChunkLoadError, scheduleChunkReload } from "@/lib/chunks";

/** errorElement of a route with a lazy chunk; any other error stays the router's. */
export function RouteError() {
  const error = useRouteError();
  if (!(error instanceof ChunkLoadError)) throw error;
  return <ChunkFailed error={error} />;
}

function ChunkFailed({ error }: { error: ChunkLoadError }) {
  // Leaving this page cancels the reload: the sidebar still works, and the next route may hold a form.
  useEffect(() => scheduleChunkReload(), []);
  return (
    <div className="px-5 py-8 md:px-10 md:py-12">
      <ErrorState
        title="This page could not be loaded."
        error={error}
        onRetry={reloadPage}
        retryLabel="Reload"
      />
    </div>
  );
}

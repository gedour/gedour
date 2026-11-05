import { reloadPage } from "@/api/client";

// Not index.html's gedour.assetReloads: its load handler clears that key before a lazy import can fail
// (the route waits for the session), so a shared counter would never reach its limit.
const KEY = "gedour.chunkReloads";
const MAX_RELOADS = 4;

export class ChunkLoadError extends Error {
  constructor(cause: unknown) {
    super("The app may have been updated, or the connection dropped. Reload to try again.", {
      cause,
    });
  }
}

export function chunkLoaded() {
  try {
    sessionStorage.removeItem(KEY);
  } catch {
    // Blocked storage holds no counter.
  }
}

/** Arms the next reload, at 2, 4, 8 then 16 s; returns its cancel, or nothing once the tries are spent. */
export function scheduleChunkReload(): (() => void) | undefined {
  let used: number;
  try {
    used = Number(sessionStorage.getItem(KEY)) || 0;
  } catch {
    // No counter means nothing would stop a permanent 404 from reloading forever.
    return undefined;
  }
  // Offline, a reload would swap this page for the browser's own error page.
  if (used >= MAX_RELOADS || !navigator.onLine) return undefined;
  const timer = setTimeout(
    () => {
      try {
        sessionStorage.setItem(KEY, String(used + 1));
      } catch {
        return;
      }
      reloadPage();
    },
    2000 * 2 ** used,
  );
  return () => clearTimeout(timer);
}

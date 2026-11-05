import { reloadPage, serverReachable } from "@/api/client";

// Not index.html's gedour.assetReloads: its load handler clears that key before a lazy import can fail
// (the route waits for the session), so a shared counter would never reach its limit.
const KEY = "gedour.chunkReloads";
const MAX_RELOADS = 4;
const MAX_WAIT_MS = 30_000;
// Far above the longest gap of a series, so only a later rollout starts a new one.
const FORGET_AFTER_MS = 5 * 60_000;

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

/** Reloads already spent in this series; throws when storage is blocked. */
function reloadsUsed(): number {
  const stored = sessionStorage.getItem(KEY);
  try {
    const { count, at } = JSON.parse(stored ?? "{}") as { count?: number; at?: number };
    return Date.now() - (at ?? 0) > FORGET_AFTER_MS ? 0 : (count ?? 0);
  } catch {
    return 0;
  }
}

/**
 * Arms the next reload, at 2, 4, 8 then 16 s. A server that does not answer costs no try: the browser's
 * error page would replace this one, so it waits and asks again. Returns the cancel, or nothing once the
 * tries are spent.
 */
export function scheduleChunkReload(): (() => void) | undefined {
  let used: number;
  try {
    used = reloadsUsed();
  } catch {
    // No counter means nothing would stop a permanent 404 from reloading forever.
    return undefined;
  }
  if (used >= MAX_RELOADS) return undefined;

  let cancelled = false;
  let timer: ReturnType<typeof setTimeout>;
  const arm = (wait: number) => {
    timer = setTimeout(() => {
      void serverReachable().then((ok) => {
        // The user may have left while the probe ran.
        if (cancelled) return;
        if (!ok) {
          arm(Math.min(wait * 2, MAX_WAIT_MS));
          return;
        }
        try {
          sessionStorage.setItem(KEY, JSON.stringify({ count: used + 1, at: Date.now() }));
        } catch {
          return;
        }
        reloadPage();
      });
    }, wait);
  };
  arm(2000 * 2 ** used);
  return () => {
    cancelled = true;
    clearTimeout(timer);
  };
}

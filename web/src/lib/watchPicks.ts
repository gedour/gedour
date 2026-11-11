import type { Watch } from "@/types/api";

/** A picked watch: its name is null while the user does not own it; watch is known once its row was seen. */
export interface WatchPick {
  id: string;
  name: string | null;
  watch?: Watch;
}

export const LOST_WATCH = "A watch you no longer own";

export const pickLabel = (p: WatchPick) => p.name ?? LOST_WATCH;

/** 014: a body.watch_ids[i] error names the watch it is about; identical lines show once. */
export function namedScopeErrors(
  errors: Record<string, string>,
  sent: WatchPick[],
): Record<string, string> {
  const seen = new Set<string>();
  return Object.fromEntries(
    Object.entries(errors).flatMap(([k, msg]) => {
      const at = /^watch_ids\[(\d+)\]$/.exec(k)?.[1];
      const pick = at === undefined ? undefined : sent[Number(at)];
      if (!pick) return [[k, msg]];
      const line = `${pickLabel(pick)}: ${msg}`;
      // Two lost watches read the same: one line says it.
      if (seen.has(line)) return [];
      seen.add(line);
      return [[k, line]];
    }),
  );
}

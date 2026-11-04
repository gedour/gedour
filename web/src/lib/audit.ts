// Target types that exist so far; later phases add theirs here.
export const TARGET_TYPES = [
  { value: "watch", label: "Watch" },
  { value: "watch_share", label: "Watch share" },
  { value: "credential", label: "Credential" },
  { value: "user", label: "User" },
  { value: "session", label: "Session" },
];

/** Badge colours by verb: creations green, removals red, the rest neutral. */
export function actionTone(action: string): string {
  const verb = action.slice(action.lastIndexOf(".") + 1);
  if (["created", "login", "added"].includes(verb))
    return "bg-emerald-500/12 text-emerald-800 ring-emerald-600/25 dark:text-emerald-300";
  if (["deleted", "logout", "removed"].includes(verb))
    return "bg-rose-500/12 text-rose-800 ring-rose-600/25 dark:text-rose-300";
  return "bg-beacon-soft text-ink ring-beacon/30";
}

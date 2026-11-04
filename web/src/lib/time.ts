const UNITS: [Intl.RelativeTimeFormatUnit, number][] = [
  ["year", 365 * 24 * 3600],
  ["month", 30 * 24 * 3600],
  ["week", 7 * 24 * 3600],
  ["day", 24 * 3600],
  ["hour", 3600],
  ["minute", 60],
];

const relative = new Intl.RelativeTimeFormat("en", { numeric: "auto" });

/** "3 minutes ago", "yesterday", "just now". */
export function formatRelative(date: Date, now: Date = new Date()): string {
  const secs = Math.round((date.getTime() - now.getTime()) / 1000);
  for (const [unit, size] of UNITS) {
    if (Math.abs(secs) >= size) return relative.format(Math.trunc(secs / size), unit);
  }
  return "just now";
}

const absolute = new Intl.DateTimeFormat("en", { dateStyle: "medium", timeStyle: "medium" });

export function formatAbsolute(date: Date): string {
  return absolute.format(date);
}

/** Start of a local calendar day (yyyy-mm-dd) as an ISO instant; with nextDay, the start of the day after. */
export function dayStartISO(day: string, nextDay = false): string {
  const d = new Date(`${day}T00:00:00`);
  if (nextDay) d.setDate(d.getDate() + 1);
  return d.toISOString();
}

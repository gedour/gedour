import type { InboxItem } from "@/types/api";

export interface DayGroup {
  /** "Today", "Yesterday" or a date such as "Mon, Nov 2". */
  label: string;
  items: InboxItem[];
}

function dayKey(d: Date): string {
  return `${d.getFullYear()}-${d.getMonth()}-${d.getDate()}`;
}

/** Groups items (newest first) by local arrival day, keeping their order. */
export function groupByDay(items: InboxItem[], now: Date = new Date()): DayGroup[] {
  const today = dayKey(now);
  const y = new Date(now);
  y.setDate(y.getDate() - 1);
  const yesterday = dayKey(y);
  const dated = new Intl.DateTimeFormat("en", { weekday: "short", month: "short", day: "numeric" });
  const datedYear = new Intl.DateTimeFormat("en", { dateStyle: "medium" });
  const groups: DayGroup[] = [];
  const byKey = new Map<string, DayGroup>();
  for (const it of items) {
    const at = new Date(it.created_at);
    const key = dayKey(at);
    let g = byKey.get(key);
    if (!g) {
      let label: string;
      if (key === today) label = "Today";
      else if (key === yesterday) label = "Yesterday";
      else if (at.getFullYear() === now.getFullYear()) label = dated.format(at);
      else label = datedYear.format(at);
      g = { label, items: [] };
      byKey.set(key, g);
      groups.push(g);
    }
    g.items.push(it);
  }
  return groups;
}

/** Wait before the n-th reconnect attempt (from 0): 1 s doubling up to 30 s. */
export function reconnectDelay(attempt: number): number {
  return Math.min(1000 * 2 ** attempt, 30_000);
}

export interface StreamHandlers {
  /** The unread count, on connect and on every change. */
  onCount: (unread: number) => void;
  /** The stream dropped (once per drop): refetch, something may have arrived meanwhile. */
  onDrop: () => void;
  /** The session ended while streaming. */
  onSignedOut: () => void;
}

export const STREAM_URL = "/api/v1/inbox/stream";

// EventSource.CLOSED as a literal: this module also loads where the global is missing (jsdom).
const CLOSED = 2;

/**
 * Follows the inbox stream: the browser reconnects a dropped stream by itself; a refused one
 * (429, 5xx) is closed for good, so it is reopened here with backoff. Returns the disconnect function.
 */
export function connectInbox(
  h: StreamHandlers,
  open: (url: string) => EventSource = (url) => new EventSource(url),
): () => void {
  let es: EventSource | null = null;
  let attempt = 0;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let stopped = false;
  let dropped = false;
  const connect = () => {
    es = open(STREAM_URL);
    es.addEventListener("inbox", (e) => {
      attempt = 0;
      dropped = false;
      const { unread } = JSON.parse((e as MessageEvent<string>).data) as { unread: number };
      h.onCount(unread);
    });
    es.addEventListener("signed_out", () => {
      stopped = true;
      es?.close();
      h.onSignedOut();
    });
    es.onerror = () => {
      if (!dropped) h.onDrop();
      dropped = true;
      if (es?.readyState !== CLOSED || stopped) return;
      timer = setTimeout(connect, reconnectDelay(attempt++));
    };
  };
  connect();
  return () => {
    stopped = true;
    clearTimeout(timer);
    es?.close();
  };
}

import type { Channel, Delivery, InboxItem, PreviewItem, Rule, WatchEvent } from "@/types/api";

export function event(over: Partial<WatchEvent> = {}): WatchEvent {
  return {
    id: 1,
    type: "release.published",
    occurred_at: null,
    observed_at: "2026-11-04T10:00:00Z",
    payload: { tag: "v2.104.0", url: "https://github.com/cli/cli/releases/tag/v2.104.0" },
    ...over,
  };
}

export const watchRef = { id: "w1", name: "GitHub CLI releases", kind: "github_release" as const };

export function inboxItem(id: number, over: Partial<InboxItem> = {}): InboxItem {
  return {
    id,
    created_at: new Date().toISOString(),
    read_at: null,
    archived_at: null,
    event: event({ id, payload: { tag: `v${id}.0.0` } }),
    watch: watchRef,
    ...over,
  };
}

export function channel(over: Partial<Channel> = {}): Channel {
  return {
    id: "c1",
    name: "Deploy bot",
    type: "webhook",
    config: { url: "https://hooks.example.com/gedour" },
    last4: "abcd",
    created_at: "2026-11-01T00:00:00Z",
    updated_at: "2026-11-01T00:00:00Z",
    last_delivery_status: null,
    last_delivery_at: null,
    ...over,
  };
}

export function rule(over: Partial<Rule> = {}): Rule {
  return {
    id: "r1",
    name: "Releases",
    watch_id: null,
    watch_name: null,
    filter: 'event.type == "release.published"',
    enabled: true,
    channel_ids: ["c1"],
    created_at: "2026-11-01T00:00:00Z",
    updated_at: "2026-11-01T00:00:00Z",
    ...over,
  };
}

export function delivery(id: number, over: Partial<Delivery> = {}): Delivery {
  return {
    id,
    status: "sent",
    attempts: 1,
    next_attempt_at: null,
    last_error: null,
    sent_at: "2026-11-04T10:00:00Z",
    created_at: "2026-11-04T10:00:00Z",
    channel: { id: "c1", name: "Deploy bot", type: "webhook" },
    rule: { id: "r1", name: "Releases" },
    event: event({ id }),
    watch: watchRef,
    ...over,
  };
}

export function preview(id: number, matched: boolean, error?: string): PreviewItem {
  return { event: event({ id, payload: { tag: `v${id}` } }), watch: watchRef, matched, error };
}

/** A controllable EventSource: tests emit events and errors on the last one opened. */
export class FakeEventSource {
  static instances: FakeEventSource[] = [];
  static readonly CLOSED = 2;
  readyState = 1;
  onerror: (() => void) | null = null;
  private listeners = new Map<string, ((e: MessageEvent) => void)[]>();
  constructor(readonly url: string) {
    FakeEventSource.instances.push(this);
  }
  addEventListener(name: string, fn: (e: MessageEvent) => void) {
    this.listeners.set(name, [...(this.listeners.get(name) ?? []), fn]);
  }
  emit(name: string, data: unknown) {
    for (const fn of this.listeners.get(name) ?? []) {
      fn(new MessageEvent(name, { data: JSON.stringify(data) }));
    }
  }
  fail(closed: boolean) {
    this.readyState = closed ? 2 : 0;
    this.onerror?.();
  }
  close() {
    this.readyState = 2;
  }
  static last(): FakeEventSource {
    const es = FakeEventSource.instances.at(-1);
    if (!es) throw new Error("no EventSource opened");
    return es;
  }
}

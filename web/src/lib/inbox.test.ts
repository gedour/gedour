import { afterEach, describe, expect, it, vi } from "vitest";

import { connectInbox, groupByDay, reconnectDelay, STREAM_URL } from "@/lib/inbox";
import { FakeEventSource, inboxItem } from "@/test/routing";

const open = (url: string) => new FakeEventSource(url) as unknown as EventSource;

afterEach(() => {
  FakeEventSource.instances = [];
  vi.useRealTimers();
});

describe("groupByDay", () => {
  // 001-AC20: the inbox reads as Today, Yesterday, then dates, in the items' order.
  it("labels today, yesterday, this year's and older days", () => {
    const now = new Date(2026, 10, 4, 9, 0);
    const at = (d: Date) => d.toISOString();
    const items = [
      inboxItem(5, { created_at: at(new Date(2026, 10, 4, 8, 0)) }),
      inboxItem(4, { created_at: at(new Date(2026, 10, 4, 0, 5)) }),
      inboxItem(3, { created_at: at(new Date(2026, 10, 3, 23, 59)) }),
      inboxItem(2, { created_at: at(new Date(2026, 10, 1, 12, 0)) }),
      inboxItem(1, { created_at: at(new Date(2025, 11, 31, 12, 0)) }),
    ];
    const groups = groupByDay(items, now);
    expect(groups.map((g) => [g.label, g.items.map((i) => i.id)])).toEqual([
      ["Today", [5, 4]],
      ["Yesterday", [3]],
      ["Sun, Nov 1", [2]],
      ["Dec 31, 2025", [1]],
    ]);
    expect(groupByDay([])).toEqual([]);
  });
});

describe("connectInbox", () => {
  it("backs off 1 s doubling to 30 s", () => {
    expect([0, 1, 2, 4, 5, 9].map(reconnectDelay)).toEqual([1000, 2000, 4000, 16000, 30000, 30000]);
  });

  // 001-AC19: counts flow in; a drop refetches once; a refused stream is reopened with backoff.
  it("reports counts, drops once and reopens a closed stream", () => {
    vi.useFakeTimers();
    const h = { onCount: vi.fn(), onDrop: vi.fn(), onSignedOut: vi.fn() };
    const stop = connectInbox(h, open);
    const first = FakeEventSource.last();
    expect(first.url).toBe(STREAM_URL);
    first.emit("inbox", { unread: 3 });
    expect(h.onCount).toHaveBeenLastCalledWith(3);

    // The browser retries a dropped stream itself: no second EventSource, one refetch.
    first.fail(false);
    first.fail(false);
    expect(h.onDrop).toHaveBeenCalledTimes(1);
    expect(FakeEventSource.instances).toHaveLength(1);

    // Refused (closed): reopened after 1 s, then 2 s.
    first.fail(true);
    vi.advanceTimersByTime(999);
    expect(FakeEventSource.instances).toHaveLength(1);
    vi.advanceTimersByTime(1);
    expect(FakeEventSource.instances).toHaveLength(2);
    FakeEventSource.last().fail(true);
    vi.advanceTimersByTime(1999);
    expect(FakeEventSource.instances).toHaveLength(2);
    vi.advanceTimersByTime(1);
    expect(FakeEventSource.instances).toHaveLength(3);

    // A message resets both the drop flag and the backoff.
    const third = FakeEventSource.last();
    third.emit("inbox", { unread: 4 });
    third.fail(true);
    expect(h.onDrop).toHaveBeenCalledTimes(2);
    vi.advanceTimersByTime(1000);
    expect(FakeEventSource.instances).toHaveLength(4);

    stop();
    expect(FakeEventSource.last().readyState).toBe(2);
    FakeEventSource.last().fail(true);
    vi.advanceTimersByTime(60_000);
    expect(FakeEventSource.instances).toHaveLength(4);
  });

  it("stops for good on sign-out", () => {
    vi.useFakeTimers();
    const h = { onCount: vi.fn(), onDrop: vi.fn(), onSignedOut: vi.fn() };
    connectInbox(h, open);
    const es = FakeEventSource.last();
    es.emit("signed_out", {});
    expect(h.onSignedOut).toHaveBeenCalledOnce();
    es.fail(true);
    vi.advanceTimersByTime(60_000);
    expect(FakeEventSource.instances).toHaveLength(1);
  });
});

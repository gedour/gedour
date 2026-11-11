import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { COUNT_KEY, LIST_KEY, READ_KEY, useInboxReadCount, useInboxStream } from "@/hooks/useInbox";
import { ME, mockApi } from "@/test/api";

function Probe() {
  useInboxStream();
  return null;
}

describe("useInboxStream", () => {
  // The poll is the fallback when the stream is down: a new count from it refetches the list too.
  it("refetches the list when the count changes, whatever set it", async () => {
    mockApi({ "/api/v1/inbox/count": { unread: 1 } });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    // The shell holds the stream only once the session is known.
    client.setQueryData(["me"], ME);
    const spy = vi.spyOn(client, "invalidateQueries");
    render(
      <QueryClientProvider client={client}>
        <Probe />
      </QueryClientProvider>,
    );
    await vi.waitFor(() => expect(client.getQueryData(COUNT_KEY)).toBe(1));
    expect(spy).not.toHaveBeenCalledWith({ queryKey: LIST_KEY });
    act(() => client.setQueryData(COUNT_KEY, 1));
    await new Promise((r) => setTimeout(r, 20));
    expect(spy).not.toHaveBeenCalledWith({ queryKey: LIST_KEY });
    act(() => client.setQueryData(COUNT_KEY, 3));
    await vi.waitFor(() => expect(spy).toHaveBeenCalledWith({ queryKey: LIST_KEY }));
  });
});

describe("useInboxReadCount", () => {
  // The stream carries no read count: a refresh of the list must bring it along.
  it("refetches whenever the list is refreshed", async () => {
    let read = 2;
    mockApi({ "/api/v1/inbox/count": () => Response.json({ unread: 0, read }) });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    function ReadProbe() {
      useInboxReadCount();
      return null;
    }
    render(
      <QueryClientProvider client={client}>
        <ReadProbe />
      </QueryClientProvider>,
    );
    await vi.waitFor(() => expect(client.getQueryData(READ_KEY)).toBe(2));
    read = 1;
    await act(() => client.invalidateQueries({ queryKey: LIST_KEY }));
    await vi.waitFor(() => expect(client.getQueryData(READ_KEY)).toBe(1));
  });
});

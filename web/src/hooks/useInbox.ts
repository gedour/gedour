import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef } from "react";

import { auth, inboxCount, inboxReadCount } from "@/api/client";
import { useMe } from "@/hooks/useMe";
import { connectInbox } from "@/lib/inbox";
import type { Me } from "@/types/api";

export const COUNT_KEY = ["inbox", "count"] as const;
export const LIST_KEY = ["inbox", "list"] as const;
// Under LIST_KEY: whatever refreshes the list refreshes the read count.
export const READ_KEY = [...LIST_KEY, "read"] as const;

/** Unread count for the sidebar badge; the stream keeps it live, the poll covers a stream that is down. */
export function useInboxCount() {
  return useQuery({ queryKey: COUNT_KEY, queryFn: inboxCount, refetchInterval: 60_000 });
}

/** Read items outside the archive; the stream carries no read count. */
export function useInboxReadCount() {
  return useQuery({ queryKey: READ_KEY, queryFn: inboxReadCount });
}

/** Holds the inbox stream for the whole shell (001-AC19); any count change, polled or streamed, refetches the list. */
export function useInboxStream() {
  const client = useQueryClient();
  const count = useInboxCount().data;
  // The stream serves whoever the session runs as when it opens: act-as start and stop reopen it.
  const userId = (useMe().data as Me).id;
  const last = useRef<number | undefined>(undefined);
  useEffect(() => {
    if (last.current !== undefined && count !== undefined && count !== last.current) {
      void client.invalidateQueries({ queryKey: LIST_KEY });
    }
    last.current = count;
  }, [client, count]);
  useEffect(() => {
    if (typeof EventSource === "undefined") return;
    // The count sent on (re)connect is news only when it differs from what we show.
    let fresh = true;
    return connectInbox({
      onCount: (n) => {
        const prev = client.getQueryData<number>(COUNT_KEY);
        client.setQueryData(COUNT_KEY, n);
        // A changed count refetches through the effect above; an unchanged one after a live event
        // (one item in, one read elsewhere) still may hide new items.
        if (!fresh && prev === n) void client.invalidateQueries({ queryKey: LIST_KEY });
        fresh = false;
      },
      onDrop: () => {
        fresh = true;
        void client.invalidateQueries({ queryKey: ["inbox"] });
      },
      // 002-AC4: a 401 on this check signs the app out.
      onSessionCheck: () => void client.invalidateQueries({ queryKey: ["me"] }),
      onSignedOut: () => auth.set(true),
    });
  }, [client, userId]);
}

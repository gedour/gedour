import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { useNavigate } from "react-router";

import { listChannelTypes, listPushDevices } from "@/api/client";
import { problemText } from "@/lib/problems";
import { addThisBrowser, currentEndpointHash, pushBlocked, PushRefused } from "@/lib/push";
import type { PushDevice } from "@/types/api";

export const DEVICES_KEY = ["push-devices"];
const ENDPOINT_KEY = ["push-endpoint"];

/** The caller's devices, and this browser's device id: null when it is none, undefined while unknown. */
export function usePushDevices() {
  const devices = useQuery({ queryKey: DEVICES_KEY, queryFn: listPushDevices });
  const hash = useQuery({ queryKey: ENDPOINT_KEY, queryFn: currentEndpointHash });
  const thisId =
    devices.data && hash.isSuccess
      ? (devices.data.find((d) => d.endpoint_sha256 === hash.data)?.id ?? null)
      : undefined;
  return { devices, thisId };
}

/** Why Web Push cannot be used here: the server's hint, or this browser's limits (010-AC14). */
export function usePushUnavailable(): string | null {
  const types = useQuery({
    queryKey: ["channel-types"],
    queryFn: listChannelTypes,
    staleTime: Infinity,
  });
  const t = types.data?.find((x) => x.type === "web_push");
  if (t && !t.available) return t.hint ?? "Web Push is off on this server.";
  return pushBlocked();
}

/**
 * Registers this browser. add() must be called from the user's click or change handler: it asks for
 * permission before any other await (010-AC17).
 */
export function useAddBrowser(onAdded?: (d: PushDevice) => void) {
  const client = useQueryClient();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const add = (name: string) => {
    setError("");
    setBusy(true);
    addThisBrowser(name)
      .then((d) => {
        client.setQueryData<PushDevice[]>(DEVICES_KEY, (old) => [
          ...(old ?? []).filter((x) => x.id !== d.id),
          d,
        ]);
        void client.invalidateQueries({ queryKey: DEVICES_KEY });
        void client.invalidateQueries({ queryKey: ENDPOINT_KEY });
        onAdded?.(d);
      })
      .catch((err: unknown) => {
        // Refused permission or a failed registration: re-check, the subscription may exist anyway.
        void client.invalidateQueries({ queryKey: ENDPOINT_KEY });
        setError(err instanceof PushRefused ? err.message : problemText(err));
      })
      .finally(() => setBusy(false));
  };
  return { add, error, busy };
}

/**
 * Routes in place to the link of a clicked notification, which public/sw.js posts to the open tab
 * (010-AC13); a URL off this origin is ignored.
 */
export function useNotificationLinks() {
  const navigate = useNavigate();
  useEffect(() => {
    const sw = navigator.serviceWorker;
    if (!sw) return;
    const onMessage = (e: MessageEvent) => {
      const raw: unknown = (e.data as { url?: unknown } | null)?.url;
      if (typeof raw !== "string") return;
      const here = new URL(document.baseURI).origin;
      let url: URL;
      try {
        url = new URL(raw, here);
      } catch {
        return;
      }
      if (url.origin === here) void navigate(url.pathname + url.search + url.hash);
    };
    sw.addEventListener("message", onMessage);
    return () => sw.removeEventListener("message", onMessage);
  }, [navigate]);
}

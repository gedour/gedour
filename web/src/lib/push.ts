import { getPushKey, registerPushDevice } from "@/api/client";
import type { PushDevice } from "@/types/api";

export const NO_PUSH =
  "This browser has no Web Push: use a recent Chrome, Edge, Firefox or Safari (on iPhone and iPad, Safari only offers it to sites added to the Home Screen).";
export const PUSH_DENIED =
  "Notifications are blocked for this site: allow them in the browser's site settings, then add this browser.";
export const PUSH_DISMISSED = "Notifications were not allowed: add this browser to ask again.";

/** Why this browser cannot be added now (010-AC14), or null when it can. */
export function pushBlocked(): string | null {
  if (!("serviceWorker" in navigator) || !("PushManager" in window) || !("Notification" in window))
    return NO_PUSH;
  return Notification.permission === "denied" ? PUSH_DENIED : null;
}

/** A readable default device name: "Firefox on Linux". */
export function defaultDeviceName(ua = navigator.userAgent): string {
  const browser = /Edg\//.test(ua)
    ? "Edge"
    : /Firefox\//.test(ua)
      ? "Firefox"
      : /Chrome\//.test(ua)
        ? "Chrome"
        : /Safari\//.test(ua)
          ? "Safari"
          : "Browser";
  const os = /Android/.test(ua)
    ? "Android"
    : /iPhone|iPad/.test(ua)
      ? "iOS"
      : /Mac OS X/.test(ua)
        ? "macOS"
        : /Windows/.test(ua)
          ? "Windows"
          : /Linux/.test(ua)
            ? "Linux"
            : "";
  return os ? `${browser} on ${os}` : browser;
}

async function sha256Hex(s: string): Promise<string> {
  const sum = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(s));
  return Array.from(new Uint8Array(sum), (b) => b.toString(16).padStart(2, "0")).join("");
}

/** The hex SHA-256 of this browser's push endpoint, matched against devices' endpoint_sha256; null if none. */
export async function currentEndpointHash(): Promise<string | null> {
  if (pushBlocked() === NO_PUSH) return null;
  const reg = await navigator.serviceWorker.getRegistration("/");
  const sub = await reg?.pushManager.getSubscription();
  return sub ? sha256Hex(sub.endpoint) : null;
}

export class PushRefused extends Error {}

/**
 * Asks for permission, subscribes and registers this browser. Call it straight from a click or change
 * handler: requestPermission must run before any other await to keep the gesture.
 */
export async function addThisBrowser(name: string): Promise<PushDevice> {
  const blocked = pushBlocked();
  if (blocked) throw new PushRefused(blocked);
  const permission = await Notification.requestPermission();
  if (permission !== "granted")
    throw new PushRefused(permission === "denied" ? PUSH_DENIED : PUSH_DISMISSED);
  await navigator.serviceWorker.register("/sw.js");
  const reg = await navigator.serviceWorker.ready;
  const sub = await reg.pushManager.subscribe({
    userVisibleOnly: true,
    applicationServerKey: await getPushKey(),
  });
  const { keys } = sub.toJSON();
  return registerPushDevice({
    endpoint: sub.endpoint,
    keys: { p256dh: keys?.p256dh ?? "", auth: keys?.auth ?? "" },
    name,
    user_agent: navigator.userAgent,
  });
}

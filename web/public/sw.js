// gedour service worker (spec 010): shows pushed channel deliveries, opens gedour on a click.
// Plain script, served as is at /sw.js: its scope is the whole origin.

// No fetch handler, no caches: an update can take over under a live page instead of waiting for every tab to close.
self.addEventListener("install", () => self.skipWaiting());

function showPush(data) {
  let m = {};
  try {
    m = data ? data.json() : {};
  } catch {
    // not JSON: show the default title
  }
  return self.registration.showNotification(m.title || "gedour", {
    body: m.body || "",
    tag: m.tag || undefined,
    silent: m.silent === true,
    // Not the SVG favicon: Chrome resolves showNotification yet drops some notifications with an SVG icon.
    icon: "/icon-192.png",
    data: { url: m.url || "/inbox" },
  });
}

// Security: only a path on this origin is opened, whatever the payload says.
function targetURL(notification) {
  const url = new URL(notification.data?.url || "/inbox", self.location.origin);
  return url.origin === self.location.origin
    ? url.href
    : new URL("/inbox", self.location.origin).href;
}

// Tabs the SPA does not run in: a message there is heard by nobody.
const NOT_SPA = /^\/(api\/|mcp(\/|$)|sw\.js$)/;

// An open gedour tab routes in place, without a reload (the SPA listens for this message).
async function openNotification(notification) {
  notification.close();
  const url = targetURL(notification);
  const windows = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
  const win = windows.find((w) => !NOT_SPA.test(new URL(w.url).pathname));
  if (!win) {
    await self.clients.openWindow(url);
    return;
  }
  await win.focus().catch(() => {});
  win.postMessage({ url });
}

self.addEventListener("push", (event) => event.waitUntil(showPush(event.data)));
self.addEventListener("notificationclick", (event) =>
  event.waitUntil(openNotification(event.notification)),
);

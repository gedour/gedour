# 010 — Web Push channel

**Status**: in progress
**Requested by**: babs
**Date**: 2026-11-07

## Problem

Every channel today needs a third-party account or server: a Telegram bot, a Nextcloud instance, a mailbox, a
webhook receiver. A user who just wants a notification on the laptop or phone where gedour is already open
has no direct way to get one.

## Solution

A user registers each browser where they want notifications as a device, from the gedour UI: one click, the
browser asks for permission, the device gets a name ("Work laptop"). A new channel type, Web Push, sends
native browser notifications to the user's devices: either all of them (devices added later included) or the
ones picked in the channel form. Clicking a notification opens gedour on the event.

## Scope

- Devices: a user registers, renames and removes push subscriptions of their own browsers. Each device keeps
  its name, the browser's user agent at registration, and when it last received a push.
- `web_push` channel type: config `devices: "all"` or a list of the owner's device ids, plus `silent` (as
  Telegram and Talk, spec 003).
- Delivery: one gedour delivery fans out to every targeted device. A retry sends only to the devices that
  have not accepted it yet.
- Urgency header: `low` for a silent channel, `high` for an event taken by a high priority rule (spec 005),
  `normal` otherwise. Silent also sets the notification's `silent` option.
- A device the push service reports gone (404 or 410) is removed, and the removal is audited.
- A server VAPID key pair, generated at first start and sealed with `GEDOUR_ENCRYPTION_KEY`; its public key
  is served to signed-in users.
- Push encryption (RFC 8291, `aes128gcm`) and VAPID (RFC 8292) with the Go standard library.
- Service worker at `/sw.js`: shows the notification, opens gedour on click. CSP gains `worker-src 'self'`.
- Permission is asked only on a user gesture: picking the Web Push type in the new channel form, when the
  current browser is not a device yet and its permission is still `default`, or a click on "Add this
  browser" (device list and channel form, the fallback after a dismissed prompt). Saving a channel never
  prompts: it can target devices registered from other browsers.
- "Send test" on a Web Push channel pushes to its devices.
- API, OpenAPI, channel form, device list, README.

## Out of scope

- Push on iOS/iPadOS: it needs gedour installed to the home screen (a web app manifest); a later spec.
- VAPID key rotation: a new key invalidates every subscription.
- Rich notifications (actions, images), per-device quiet hours, notification grouping.
- Sharing a device with another user, or targeting another user's devices.
- Pushing anything other than channel deliveries (inbox counter, system messages).

## Acceptance criteria

- [ ] **AC1** A signed-in user registers the current browser as a named device; the API refuses a
  subscription endpoint that is not `https`, or whose host is a literal private IP unless
  `GEDOUR_ALLOW_PRIVATE_NETS` allows it (400); a hostname resolving to a private address is refused by
  the dial-time guard at send, as for webhooks.
- [ ] **AC2** A user lists, renames and removes only their own devices; another user's device id is a 404.
- [ ] **AC3** Registering the same endpoint twice updates the existing device, never makes a second one.
- [ ] **AC4** A Web Push channel with `devices: "all"` delivers to every device of its owner, including one
  registered after the channel was created.
- [ ] **AC5** A channel with a device list delivers only to those devices; a list naming a device that is
  not the owner's is a 400; removing a device drops it from every list that names it.
- [ ] **AC6** The pushed payload decrypts with the subscription's keys (RFC 8291 test vector, and a
  round trip against the fake push service), and the VAPID JWT verifies against the served public key.
- [ ] **AC7** A delivery where one device accepts and another fails retries only the failed device; it is
  `sent` once every targeted device has accepted or been removed.
- [ ] **AC8** A 404 or 410 from the push service removes the device and writes an audit entry; the delivery
  does not retry that device.
- [ ] **AC9** A channel with no device to send to (none registered, or every listed device removed) makes its
  deliveries `dead` with the error "no device", shown as the channel's last status.
- [ ] **AC10** Urgency is `low` for a silent channel, `high` for an event of a high priority rule, `normal`
  otherwise; silent wins when both apply.
- [ ] **AC11** The VAPID key pair survives a restart and is stored sealed, never in clear in the database.
- [ ] **AC12** `/sw.js` is served with a JavaScript content type, not the SPA fallback; the CSP allows it and
  e2e sees no CSP violation.
- [ ] **AC13** In Chromium with notifications granted, an e2e test registers a device, creates a channel,
  sends a test, and the service worker shows the notification; clicking it opens gedour.
- [ ] **AC14** A browser that refuses permission, or has no Push API, shows why in the form and registers
  nothing.
- [ ] **AC15** The device list and channel form pass the a11y (light and dark), 375 px, keyboard and CSP checks.
- [ ] **AC16** Device registration, rename and removal, and Web Push channel changes, are in the audit log;
  the endpoint and keys never appear in it or in the API's channel output.
- [ ] **AC17** In the new channel form, picking the Web Push type asks for permission when the current browser
  is not a device and its permission is `default`; "Add this browser" stays as the fallback. No prompt on page
  load or on save. Once granted, the new device is registered and selected in the form.

## Phases

### Phase 1 — Push sender
- Work: VAPID key generation and storage, RFC 8291 encryption, VAPID JWT, the push HTTP request with
  Urgency and TTL, error mapping (404/410 gone, 400/401/403/413 permanent, 429/5xx/other retry), a push receiver in
  `cmd/fakeupstream` that decrypts and records.
- **Covers**: AC6, AC11
- **Data model impact**: new table holding the sealed VAPID key pair (single row).
- **DoD**: `go test ./pkg/notify/...` green with the RFC 8291 vector and a fake-service round trip;
  `make coverage` passes.

### Phase 2 — Devices and the channel type
- Work: device API (register, list, rename, delete), `web_push` channel type and config validation,
  fan-out with per-device progress, removal on gone, urgency, audit, OpenAPI.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC7, AC8, AC9, AC10, AC16
- **Data model impact**: `push_devices` (owner, name, endpoint unique, sealed keys, user agent, last push);
  a per-delivery table of devices already sent; `channels_type_check` gains `web_push`.
- **DoD**: `make coverage` green; route tests cover partial failure, gone devices and an empty target set.

### Phase 3 — Service worker and UI
- Work: `/sw.js`, CSP `worker-src`, "Register this browser" and the device list, the channel form with
  all / picked devices and silent, README.
- **Covers**: AC12, AC13, AC14, AC15, AC17
- **Data model impact**: none
- **DoD**: `make e2e` green including the push spec; `make lint` clean.

## Data model impact (summary)

- New: VAPID key table (one sealed row), `push_devices`, a per-delivery sent-device table.
- Changed: `channels_type_check` adds `web_push`.
- `notify.Rule` carries the rule priority, so the sender can set Urgency.

## Open questions

- [ ] None blocking. The decisions below are open to change.

## Decisions

- Devices belong to the user, channels point at them: one browser serves several channels, and "all" covers
  new devices without editing channels. Because that is the model chosen on 2026-11-07.
- Push endpoints and keys are sealed like channel secrets: the endpoint is a bearer capability for that
  browser. Personal data stays what channels already hold (a user agent string is added), so the auth model
  is unchanged.
- The VAPID key is generated, not configured: no new required variable. It is sealed with `key_version` like
  channel secrets; a changed `GEDOUR_ENCRYPTION_KEY` makes it unreadable, which disables Web Push (logged)
  without stopping the service.
- A gone device is deleted, not flagged: it can never receive again, and the browser re-registers on demand.
- Partial success is tracked per device, so a retry never repeats a notification on a device that got it.
- The endpoint goes through the same network guard as webhooks: it is a URL from the browser, so a user could
  point it at an internal host.
- The notification text reuses the Telegram/Talk message (title and first lines), trimmed to stay under the
  4 KB push payload limit.
- iOS is out: it needs a manifest and home-screen install, which changes the app shell.

## Notes: iOS / iPadOS

- Web Push works since iOS/iPadOS 16.4, only in a web app added to the Home Screen; a Safari tab has no
  `PushManager`.
- iOS 16.4 to 18: the site opens as a web app only with a manifest `display: standalone`/`fullscreen` (or the
  legacy `apple-mobile-web-app-capable` meta). iOS 26: any site added with "Open as Web App" qualifies; a
  manifest still gives name, icons, `start_url`, `scope`, `id`.
- The prompt needs a user gesture (`NotAllowedError` otherwise), which AC17 already enforces.
- iOS 18.4 adds Declarative Web Push (no service worker); the standard service-worker path still works.
- What a later spec needs: a `manifest.webmanifest` (served under the CSP, no data: icons), PNG icons,
  `<link rel="manifest">`, and an "install to Home Screen first" hint in the form on iOS without
  standalone mode. The server side of this spec is unchanged.
- Sources: [progressivewebapps.com: iOS web push](https://progressivewebapps.com/background/ios-web-push/),
  [openpwa.net: iOS Add to Home Screen](https://openpwa.net/reference/installation/ios-add-to-home-screen/),
  [Notificare: Web Push in iOS Safari](https://notificare.com/blog/2023/03/27/web-push-in-ios-safari/).

# 021 — Mark read on upstream open

**Status**: in progress
**Requested by**: babs
**Date**: 2026-11-22

## Problem

A Telegram, Nextcloud Talk or email message links to the upstream page ("Open upstream") and, since 017,
to the event in gedour ("Open in gedour"). A user who reads the upstream page from the message still has
the event unread in gedour. Marking it read takes a second click on "Open in gedour", then a third to
reach upstream from there. On a phone, in Telegram's in-app browser, gedour usually has no session at all.

## Solution

A channel setting, off by default, makes every upstream link of the channel's messages go through
gedour. Opening the link shows a blank gedour page for a moment, marks the event read in the channel
owner's inbox, then lands on the upstream page. No sign-in is needed: the link carries a signature that
gedour checks. The signature sits in the URL fragment, which browsers never send to a server: a client
that fetches the link without running its script gets an empty page and changes nothing.

## Scope

- Telegram, Nextcloud Talk and email channel config gets `read_on_open` (boolean, `false` when absent).
- When it is on and `GEDOUR_PUBLIC_URL` is set, every upstream link a single-event message, a grouped
  message (017), a digest (012) or a rule live test (013) carries points to `<public URL>/r#<link>`
  instead. An event without a URL or with a non-http(s) one gets no link, as today; with the swap on
  (option and public URL), neither does one `url.Parse` refuses or reads without a host. The
  renderers are unchanged apart from that URL string: their caps (Telegram's 4096 and title-only
  fallback, Talk's `maxTalkLink` and `maxTalkLen`, digest cuts) apply to the signed link as rendered,
  never to the URL it replaces. `<public URL>` is the first entry of
  `GEDOUR_PUBLIC_URL`, as for the 017 link; the worker reads it too, so the README lists it for both roles.
- Link format, fixed for good (no expiry, no version):
  - `<link>` = `<event id>.<owner id>.<tag>`: event id in decimal, owner id the canonical lowercase UUID.
  - `<tag>` = the first 16 bytes of HMAC-SHA256 over the ASCII `<event id>.<owner id>`, `base64.RawURLEncoding`.
  - HMAC key = `hkdf.Key(sha256.New, secret.ParseKey(GEDOUR_ENCRYPTION_KEY), nil, "gedour read-on-open v1", 32)`.
    No new setting. The handler re-computes the expected `<tag>` string and compares it with the received
    one through `hmac.Equal` (no decoding: base64's unused trailing bits cannot alias a tag); a fixed test
    vector pins the format. A sender without a key swaps no link, and verification without one is a 404.
- The owner id is read with the channel: `loadChannel` selects `owner_id` into a new `notify.Channel`
  field, which covers single sends, groups, digests and the live test.
- `GET /r` serves one static page, the same bytes for every link, holding no event data; `GET /r.js`
  serves its script, within the CSP `script-src 'self'`. Both are `go:embed` files in `pkg/server` with
  explicit routes, so they work without a frontend build and never fall back to the SPA.
- `POST /api/v1/open`, a huma operation listed in `auth.publicAPI` (no session, no CSRF token: the tag is
  the credential). Body `{"link": string}`, no `maxLength` or pattern: length and parsing are the
  handler's, so every string that does not verify gets the same 404. The operation's `MaxBodyBytes` is
  1 KiB (huma's 413 over it). Answer 200 `{"url": string}` with `Cache-Control: no-store`.
- A valid link, in one transaction: reads `events.payload->>'url'` where the owner is active
  (`activeOwner`: `disabled_at` and `purged_at` null) and `watch.ReadableSQL` holds for the owner; marks
  the item read with `user_id = owner AND event_id = id AND read_at IS NULL` plus those same conditions in
  the `UPDATE`'s `WHERE`, archived items included (as `POST /api/v1/inbox/read`); tells the owner's open
  tabs the new count when it changed (as that operation). No inbox item (a live test of an event the
  owner never got): the URL, nothing changed.
- The upstream URL is that stored `url`, re-checked as at signing (http(s) prefix, `url.Parse` with a
  host); a failure is the same 404. Nothing from the request picks it.
- The page script reads `location.hash`, posts it without its leading `#`, and calls `location.replace` with the answer's URL.
  On 429, a 5xx or a network failure it says gedour could not be reached, try again; on any other
  non-2xx (404, 400, 413) it says the link is not valid. Both show an inbox link. Without JavaScript, a `<noscript>` line links to the inbox.
- The "Open in gedour" link (017) and the digest inbox links are unchanged.
- The service worker's `NOT_SPA` (`web/public/sw.js`) gains `/r` and `/r.js`, so a Web Push click never
  routes into a `/r` tab.
- Channel form: checkbox "Mark read when I open upstream", unchecked by default, on the three types, with
  a hint that it needs `GEDOUR_PUBLIC_URL` and that anyone who opens the link marks the item read.
- API doc string of the channel config, OpenAPI regenerated, README channels section (same caveat, plus
  mail sandboxes that run JavaScript), AGENTS.md: SPA fallback exceptions (`/r`, `/r.js`) and `/r.js`'s `location.replace` as a second
  script navigation outside ESLint's reach.

## Out of scope

- Webhook and script channels: they carry the raw event.
- Web Push: a click already opens the event in gedour, which marks it read (010-AC13).
- Link expiry, per-link revocation, a click log, links bound to a channel.
- Keeping links valid across a change of `GEDOUR_ENCRYPTION_KEY`: a new key refuses old links.
- Telling a headless browser that runs the script apart from a human (`Sec-Fetch-User` and the like).
- A no-JavaScript path to upstream: the server never sees the tag on `GET`.
- Requiring an https public URL.

## Acceptance criteria

- [ ] **AC1** With `read_on_open` absent or `false`, the rendered text of single, grouped and digest
  messages is byte-identical to before this spec: Telegram and Talk text, email subject and both decoded
  MIME parts.
- [ ] **AC2** With `read_on_open: true` and `GEDOUR_PUBLIC_URL` set, every upstream link rendered in a
  single, grouped, digest or live test message on Telegram, Talk and email is
  `<first public URL>/r#<event id>.<owner id>.<tag>`, `<owner id>` the channel owner's; the "Open in
  gedour" and inbox links are unchanged; the length caps, whole-line cuts, "+N more" and inbox link of
  012/017 apply to the text as rendered with signed links, and a message the Telegram fallback cuts to
  its title has no upstream link.
- [ ] **AC3** With `read_on_open: true` and no `GEDOUR_PUBLIC_URL`, messages are as in AC1.
- [ ] **AC4** An event without an upstream URL or with a non-http(s) one gets no upstream link, as
  today. With `read_on_open` on and a public URL set, neither does one with an http(s) prefix that `url.Parse` refuses or reads
  without a host, and a Talk event whose URL is over `maxTalkLink` gets its signed link (the cap applies
  to the rendered link).
- [ ] **AC5** A channel saved with a non-boolean `read_on_open`, or `read_on_open` on another channel
  type, is a 400; `null` reads as absent (off), as `gedour_link`'s `null` does; `read_on_open: true`
  saved on each of the three types reads back true from `GET /api/v1/channels/{id}`.
- [ ] **AC6** `GET /r` answers 200 with the same body for any request, no event data, no state change;
  `GET /r.js` answers the script; `Content-Type` is `text/html` and `text/javascript` (responses carry
  `nosniff`); neither returns the SPA, with or without a frontend build.
- [ ] **AC7** `POST /api/v1/open` with a valid link and no session: the owner's unread item gets
  `read_at`, the owner's open inbox stream gets the new count, the answer is 200 `{"url"}` with the
  stored upstream URL and `Cache-Control: no-store`.
- [ ] **AC8** A valid link whose item is already read keeps its first `read_at` and still answers the URL;
  an archived unread item gets `read_at` and stays archived; a readable event with no inbox item for the
  owner answers the URL and changes nothing.
- [ ] **AC9** A body without `link`, a non-string `link` or malformed JSON is a huma 400; a body over
  1 KiB a 413. Any string that does not verify (changed tag, event id or owner id, malformed, too long, a
  tag from another `GEDOUR_ENCRYPTION_KEY`, an unknown event, a stored `url` that fails the re-check)
  answers the same 404, no URL, no state change, no query run before the tag checks. A fixed test vector
  (key, event id, owner id, expected link) passes; a tag whose last character differs only in its unused
  bits is a 404; a sender and a service without a key swap no link and answer 404.
- [ ] **AC10** A valid link answers the same 404, and changes nothing, when at the call the event's
  watch is deleted or owned by someone else, or the owner is disabled; a watch transferred back or an
  owner re-enabled makes the link work again.
- [ ] **AC11** A valid link posted by another signed-in user marks the owner's item read, never theirs.
- [ ] **AC12** The page, served under the CSP, follows a valid link to the stored upstream URL (AC14); on a 404
  or a 413 it shows the invalid-link message and an inbox link; on a 500 or a 429 the try-again message and an inbox link;
  with JavaScript off the inbox link only.
- [ ] **AC13** The channel form shows "Mark read when I open upstream" unchecked for a new Telegram, Talk
  or email channel, saves the checked state as `read_on_open: true`, and reloads it checked.
- [ ] **AC14** e2e, as bob: a Telegram channel with `read_on_open` on gets a rule live test from the rule
  page's live test button (seeded watch, `untilNotTooSoon` on 429, as `rule-live-test.spec.ts`);
  following the received message's upstream link in a browser context without a session gets 200 from
  `POST /api/v1/open` and navigates to the stored `url` (`https://github.com/**` answered by `page.route`),
  with the CSP guard (`watchCsp`) on that context. The read mark is AC7's: other bob specs open the
  newest item in parallel.
- [ ] **AC15** A Web Push click with only `/r` and `/r.js` tabs open opens a new window; a `/rules` tab
  still gets the message (`web/src/sw.test.ts`).

## Phases

### Phase 1 — Signed links, open API and page
- Work: HKDF key derived once in `pkg/app` (an error stops startup) and held by `notify.Sender`, which
  signs, and `route.Service` verifies through it; link sign/verify; `read_on_open` in the three channel configs (validation, default off,
  email normalisation keeps it); `owner_id` through `loadChannel`; sender and digest renderers swap the
  links they render when on and a public URL is set; `POST /api/v1/open` in huma and `auth.publicAPI`;
  embedded `/r` and `/r.js` with their routes; `NOT_SPA` in `sw.js`; OpenAPI (`make openapi`); README (`GEDOUR_PUBLIC_URL` for both roles); AGENTS.md.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6, AC7, AC8, AC9, AC10, AC11, AC15
- **Data model impact**: none (channel config is JSON; absent means off)
- **DoD**: `make test`, `make coverage`, `make lint`, `pre-commit run --all-files` green;
  `TestOpenAPIDrift` and `pnpm check:api` green; `go test ./pkg/notify ./pkg/route ./pkg/server -run 'ReadOnOpen'`
  covers each Go AC of the phase; `sw.test.ts` covers AC15.

### Phase 2 — Channel form and e2e
- Work: checkbox and hint in `ChannelsPage.tsx`; vitest; e2e specs as bob, one for the page (404,
  413, 429, 500 via `page.route` on `/api/v1/open`, JavaScript off) and AC14, which holds the success path.
- **Covers**: AC12, AC13, AC14
- **Data model impact**: none
- **DoD**: `make test`, `make lint` green; ChannelsPage vitest covers AC13; `make e2e` green with the new
  specs (bounded runner, 4 workers), CSP guard included.

## Data model impact (summary)

None. `read_on_open` lives in the channel's JSON config; `POST /api/v1/open` reads `events`,
`inbox_items`, `watches`, `users` and updates `inbox_items.read_at`.

## Open questions

- [x] None blocking.

## Decisions

- Signed link, not session: the click saved matters most on a phone, where gedour has no session (asked, 2026-11-22).
- All message kinds, single, grouped and digest (asked, 2026-11-22).
- Link data in the fragment, read by a script (asked, 2026-11-22): a fetcher without JavaScript changes
  nothing and learns nothing, and the tag stays out of access logs; `Referer` never carries a fragment
  and `secureHeaders` already sends `same-origin`.
- Off by default: a mail sandbox that runs the script still marks items read.
- The link binds the event and the owner, not the channel: links already sent keep working after the
  setting is turned off or the channel deleted, and anyone in a Telegram group, Talk conversation or the
  `to` list who opens one marks the owner's item read. The only effect is a read mark.
- Live test messages (013) carry signed links too: they look like a real send, and a click is the user's
  act, not an effect of the test (013-AC5 holds for the test itself).
- An http public URL is allowed: sessions already travel in clear there, and the tag only marks read.
- No check that the public URL takes a `/r#` suffix (a query or fragment in `GEDOUR_PUBLIC_URL`): the
  017 links and the OIDC callback (`<url>/auth/callback`) break the same way; config validation is
  where that belongs, not this spec.
- No row lock on the owner or the watch: a transfer or disable that commits during the call, after the
  statement's snapshot, may still answer the URL and set `read_at`. The owner already holds the URL in the
  message, and a read mark on an item the owner no longer sees changes nothing visible.
  Locking users `FOR SHARE` would make the call wait on disables for no gain.
- The mark-read call is a public huma operation under `/api`: OpenAPI and problem+json come with it; the
  `GET` pages stay outside `/api` as they serve HTML.
- No expiry: the action is a mark read; a TTL would break links in old messages for no gain.
- Same 404 for every refusal: no oracle on which events or users exist.
- Mark read is not audited, as `POST /api/v1/inbox/read` is not.
- No rate limit: a bad tag costs one HMAC and no query; a valid one, revoked or not, costs one small
  transaction, as any signed-in API call, and only the message's recipients hold one.
- Path `/r` short on purpose: it counts toward Telegram's 4096 and Talk's caps.

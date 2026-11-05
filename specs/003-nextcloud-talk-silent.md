# 003 — Nextcloud Talk channel and silent notifications

**Status**: shipped
**Requested by**: babs
**Date**: 2026-11-04

## Problem

Alerts can only reach Telegram, email, a signed webhook or a script. Teams that chat in Nextcloud Talk
have no direct channel and would need a script or a relay. And every Telegram alert buzzes the phone:
low-value watches (nightly tags, draft PR changes) cannot be delivered quietly, so they are either
muted at the chat level or not routed at all.

## Solution

A new channel type, Nextcloud Talk, posts each delivery into a Talk conversation as a bot. The
Nextcloud admin installs the bot once and enables it in the conversation; the gedour channel only needs
the Nextcloud URL, the conversation token and the bot's shared secret. Telegram and Talk channels get a
"Silent" switch: their messages arrive in the chat quietly (Telegram notifies without sound, Talk creates
no notification).

## Scope

- Channel type `nextcloud_talk`: config `url` (Nextcloud base URL) and `conversation` (room token);
  secret = the bot shared secret (encrypted like the other channel secrets).
- Delivery: `POST {url}/ocs/v2.php/apps/spreed/api/v1/bot/{conversation}/message`, headers
  `OCS-APIRequest: true`, `X-Nextcloud-Talk-Bot-Random`, `X-Nextcloud-Talk-Bot-Signature`
  (HMAC-SHA256 with the secret over the random value followed by the message, hex). Body `message`,
  `referenceId` (derived from the delivery id), `silent`.
- Message text in Markdown: title, summary, upstream link; capped below Talk's 32000-character limit.
- Response mapping into the existing delivery retry policy: 201 success; 401, 404, 413 permanent;
  everything else, 400, 429 and 5xx included, retried with backoff.
- `silent` boolean on Telegram (`disable_notification`) and Nextcloud Talk channel configs, default
  false; a switch on their channel forms only.
- Channel test button, channel form, channel list icon and label for the new type; README section with
  the Nextcloud setup (`occ talk:bot:create -s …` from Talk 22.0.1, `occ talk:bot:install --feature response …`
  before it, then `occ talk:bot:setup`).
- HTTP goes through the netguard client: a LAN-only Nextcloud needs `GEDOUR_ALLOW_PRIVATE_NETS`, as for
  webhooks.

## Out of scope

- Receiving Talk messages or reactions (webhook, event, reaction bot features): gedour only posts.
- Posting as a Nextcloud user with an app password.
- A silent switch on email, webhook or script channels, and any `silent` field in their payloads.
- Per-rule or per-event loud/silent choice.

## Acceptance criteria

- [x] **AC1** A user can create, edit, test and delete a Nextcloud Talk channel from the UI and the API;
  the bot secret is write-only (never returned, never in the audit log).
- [x] **AC2** A delivery to a Talk channel sends one signed request whose signature a Talk server
  validates, with the message text, `referenceId` and the channel's `silent` value.
- [x] **AC3** Talk answers 201 → delivery `sent`; 401/404/413 → `dead` (shown as Failed) without
  retry, the reason recorded; any other answer, 400/429/5xx/timeout included → retried per the existing policy.
- [x] **AC4** A Telegram channel with Silent on sends `disable_notification: true`; off (or unset on an
  existing channel) sends `false`.
- [x] **AC5** The Silent switch appears on Telegram and Talk channel forms only, is keyboard and
  screen-reader operable, and passes the a11y, 375 px and CSP checks.
- [x] **AC6** A Talk URL resolving to a private address is refused at send time unless allowlisted by
  `GEDOUR_ALLOW_PRIVATE_NETS`, with the same error as webhooks.
- [x] **AC7** A message longer than the cap is shortened to its title and link, never rejected; a URL
  over 30000 characters once encoded is always left out.
- [x] **AC8** After a 401 on a Talk delivery, the channel sends nothing for 30 minutes or until the channel is
  edited (name, config or secret), whichever comes first, and at most one Talk request per channel is in
  flight, about one message per `GEDOUR_WORKER_POLL`. Held deliveries keep their attempts and go out afterwards. A
  429 or 404 starts no pause, other channels on the server keep sending, and "Send test" and a manual retry of the
  refused delivery stay direct.

## Phases

### Phase 1 — Silent flag on Telegram
- Work: `silent` in the Telegram config (API, validation, form switch), `disable_notification` in the
  sender, fake-upstream capture, OpenAPI regen.
- **Covers**: AC4, AC5 (Telegram form)
- **Data model impact**: none (channel config is JSON; a missing key reads as false).
- **DoD**: `make coverage` green; an e2e spec creates a silent Telegram channel and the fake upstream
  records `disable_notification: true`.

### Phase 2 — Nextcloud Talk channel
- Work: channel type, config validation, sender with signature, retry mapping, form, list, test button,
  fake-upstream Talk endpoint that verifies the signature, README setup section.
- **Covers**: AC1, AC2, AC3, AC5 (Talk form), AC6, AC7
- **Data model impact**: new migration widening `channels_type_check` to add `nextcloud_talk`.
- **DoD**: `make coverage` and `make e2e` green, including a Talk delivery accepted by the fake's
  signature check and a 401 recorded as permanent.

## Data model impact (summary)

- `channels.type` check constraint gains `nextcloud_talk` (new migration, phase 2).
- No new column: `silent`, `url` and `conversation` live in the JSON channel config.

## Open questions

- [ ] None.

## Decisions

- Bot only, posting with the `response` feature: no Nextcloud user account involved — chosen by babs.
- Silent is a channel setting — chosen by babs; a loud and a silent channel on the same chat is the way
  to mix both.
- The signed string is the random value plus the `message` text, per the Talk bot docs' example. The
  docs leave the exact bytes ambiguous; a live post to a real room was waived by babs (2026-11-04), so the
  fake server's check follows the Talk server source, cited in the test.
- `referenceId` = SHA-256 of the delivery id, so a retried delivery is recognisable in the room.
- AC3 corrected during review: `failed` means a retry is scheduled, so a refusal that cannot succeed is `dead`,
  which the UI labels Failed; a manual retry stays possible.
- Upstream text cannot mention: `@` is escaped in titles, details and links.
- A Talk 400 is retried: gedour's request shape cannot trigger Talk's validation 400s, so the one left is a
  caught server exception (BotController::sendMessage), and it does not count for the throttler.
- AC8: Nextcloud records a `bot` bruteforce attempt per IP on every 401 and a success resets nothing. Since
  Nextcloud 32, over 10 failures in 30 minutes answer 429 to all bot posts from that IP; before that, 7 failures
  in 12 hours slow each post past gedour's 10 s timeout. The pause limits one bad channel to about 2 failing
  requests per hour. It reads `next_attempt_at` of `dead` deliveries, the time they died, and the 401 from
  `last_error`: no new column. Read from the Talk source, not observed on a live server.
- AC8 limit: at 2 failures per hour a bad channel reaches the 12-hour limit of Nextcloud 31 and older after about
  3.5 hours, so the pause only delays the slowdown there. Only a pause of 2h15 or more stays under it.

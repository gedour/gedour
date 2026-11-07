# 012 — Channel digests

**Status**: in progress
**Requested by**: babs
**Date**: 2026-11-07

## Problem

Every routed event sends one message on each channel its rule targets. A busy watch (a repository with
active PRs, nightly tags) turns into a stream of pings, and the user either mutes the chat or stops
routing the watch. Events already handled in the gedour inbox are still pushed to the phone.

## Solution

A channel can be set to "Digest" with a schedule (for example 09:00 and 18:00, Europe/Paris). Events
routed to it wait, and at each scheduled time the channel sends one message listing them, grouped by
watch. Only the events still unread in the owner's inbox at that time are listed: what the user already
read or archived in gedour is not sent again. A high priority event skips the wait and goes out at once.
A digest with nothing left to list sends nothing.

## Scope

- Channel setting `delivery`: `immediate` (today's behaviour, the default) or `digest`.
- Digest schedule in the channel config: `at`, a list of 1 to 24 local times (`HH:MM`), and `tz`, an IANA
  time zone (default `UTC`).
- Digest is offered on the Telegram, Nextcloud Talk, email and Web Push (spec 010) channel types.
- Web Push digest: one notification per scheduled time, title "N unread events", body the first watch
  names, click opens the gedour inbox; urgency `low` when silent, `normal` otherwise.
- Routing: a delivery to a digest channel is due at the channel's next scheduled time; one taken by a high
  priority rule (spec 005) is due at once and sent alone, as today.
- At send time, a waiting delivery whose event the channel owner has read or archived in the inbox is not
  sent: it ends as `skipped`, with the reason in its error text. The check runs on every attempt, so an
  event read between two retries drops out of the retry.
- One digest message per channel per scheduled time: header with the count, then events grouped by watch,
  each line the event summary and its upstream link, oldest first.
- Length cap per channel type (Telegram 4096, Talk 32000 characters); past it the message ends with
  "+N more" and a link to the gedour inbox (`GEDOUR_PUBLIC_URL`).
- Telegram and Talk `silent` (spec 003) applies to digests.
- A digest's deliveries share one outcome: sent together, or failed together with the existing backoff
  and dead after `MaxAttempts`.
- Editing the schedule moves the waiting deliveries to the new next time; switching to `immediate` makes
  them due at once (each sent alone, still filtered on read).
- "Send test" on a digest channel sends a sample digest of two events.
- API, OpenAPI, channel form (delivery mode, schedule, time zone), deliveries list showing `skipped`,
  README.

## Out of scope

- Webhook and script channels: their payload is one event; a batch format is a later spec.
- Merging several events on the same PR or issue into one line.
- Schedules other than fixed daily times (weekdays only, every N minutes).
- Filtering immediate deliveries on read state: they leave before anyone could read them.
- A per-user time zone setting.

## Acceptance criteria

- [ ] **AC1** A channel saved with `delivery: digest` and no `at`, more than 24 times, a time that is not
  `HH:MM`, or an unknown `tz` is a 400; `digest` on a webhook or script channel is a 400.
- [ ] **AC2** Given a digest channel at 09:00 and 18:00 Europe/Paris, an event routed at 10:12 Paris time
  gets a delivery due at 18:00 Paris time; one routed at 18:30 is due at 09:00 the next day. A DST change
  between routing and the scheduled time keeps the local time.
- [ ] **AC3** At a scheduled time, every due delivery of the channel goes out in one message, grouped by
  watch, oldest first; all its deliveries end `sent` with the same `sent_at`.
- [ ] **AC4** A due delivery whose inbox item for the channel owner is read or archived is not listed and
  ends `skipped`; if none is left, nothing is sent.
- [ ] **AC5** An item marked unread again before the scheduled time is listed.
- [ ] **AC6** An event taken by a high priority rule on a digest channel is sent alone at once and is not
  listed in the next digest.
- [ ] **AC7** A failed digest send retries with the existing backoff; an event read before the retry is
  skipped in it; after `MaxAttempts` all remaining deliveries of the digest are `dead`.
- [ ] **AC8** A digest longer than the channel type's cap is cut on a whole line and ends with "+N more"
  and the inbox link.
- [ ] **AC9** Changing the schedule moves waiting deliveries to the new next time; switching to
  `immediate` makes them due at once.
- [ ] **AC10** Two replicas delivering side by side never send the same digest twice nor split one digest
  into two messages.
- [ ] **AC11** The deliveries list and API show `skipped` with its reason; "Send test" on a digest channel
  sends a sample digest.
- [ ] **AC12** A Web Push digest channel sends one notification per scheduled time to its devices, titled
  with the unread count, opening the inbox on click; a retry reaches only the devices that have not
  accepted it (spec 010).

## Phases

### Phase 1 — Schedule and routing
- Work: `delivery`, `at`, `tz` in channel config with validation; next-time computation; router sets
  `next_attempt_at` for digest channels, high priority bypass; schedule edit reschedules waiting
  deliveries.
- **Covers**: AC1, AC2, AC6, AC9
- **Data model impact**: `deliveries.digest boolean NOT NULL DEFAULT false` (the delivery goes out in its channel's digest);
  `deliveries.status` CHECK gains `skipped`, unused until phase 2. `channels.config` keys need no migration.
- **DoD**: `go test ./pkg/route/...` green, with a next-time table test across a DST change.

### Phase 2 — Digest send
- Work: claim groups the due deliveries of a digest channel under one lease; read/archived filter and
  `skipped` status; digest message format and cap per type; shared outcome and retries; sample digest
  for "Send test".
- **Covers**: AC3, AC4, AC5, AC7, AC8, AC10, AC11 (API part), AC12
- **Data model impact**: none (phase 1 migration covers it).
- **DoD**: migration applies up and down on a fresh DB; `go test -race ./pkg/route/... ./pkg/notify/...`
  green, including a two-deliverer test for AC10.

### Phase 3 — UI and docs
- Work: channel form (delivery mode, times, time zone), `skipped` in the deliveries list, OpenAPI
  regenerated, README.
- **Covers**: AC11
- **Data model impact**: none
- **DoD**: vitest and the channels e2e spec green; `make` lint target green.

## Data model impact (summary)

- `deliveries.status` CHECK: add `skipped`.
- `deliveries.digest`: new boolean, true when the delivery goes out in its channel's digest; set at routing, cleared by a high-priority bypass or a switch to `immediate`.
- `channels.config`: new keys `delivery`, `at`, `tz` (no migration).

## Open questions

- [ ] None.

## Decisions

- Digest is a channel setting, not a rule setting — a user keeps an immediate and a digest channel side by
  side and each rule picks; no change to rules.
- Read state is checked at send time, not at routing — that is when "already handled" is known.
- `skipped` is a new delivery status rather than `dead` with an error — `dead` reads as a failure in the UI.
- Time zone per channel, default UTC — there is no user time zone today.
- One line per event, no merge per subject in v1 — merging needs a subject key per event type.
- Webhook and script excluded — they need a batch payload format.
- Web Push digest is a count plus watch names, not the event list — a push payload holds about 4 KB and a
  notification shows a few lines.

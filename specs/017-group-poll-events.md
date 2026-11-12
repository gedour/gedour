# 017 — One message per poll

**Status**: in progress
**Requested by**: babs
**Date**: 2026-11-12

## Problem

One watch poll can find several new events at once, such as two releases published between two polls.
Each event is sent as its own message on every channel, so the user gets several pings for one project
in the same second: two releases of one repository seen in one poll go out as 2 Telegram messages and
2 Web Push notifications.

## Solution

When one poll of a watch yields several events routed at once to the same channel, the channel gets
one message listing them all. A poll that finds a single event still sends today's message. No timer
is added: the grouped message leaves once its poll is routed and its events are due.

Update (2026-11-12, phase 2): Telegram, Nextcloud Talk and email messages also carry a link to gedour. A
message about one event links to its inbox entry; a grouped message or a digest links to the inbox once,
at the end. A channel setting, on by default, turns the link off.

## Scope

- Grouping applies to deliveries sent at once (not digest, spec 012) on Telegram, Nextcloud Talk, email
  and Web Push channels.
- Group key: same channel, same watch, same poll (the events' `observed_at`, written once per poll),
  same priority.
- A poll's grouped deliveries wait until every event of the poll is routed (one router pass in
  practice), so a poll split across router passes still goes out as one message.
- One event in the group: today's single message, unchanged.
- Two or more: one message in the 012 digest layout, the header reading "N new events" (not "unread"),
  events oldest first, each line its summary and upstream link; the 012 length caps and "+N more" apply.
- Web Push: one notification, title "N new events", body the watch name, click opens the inbox.
- Telegram and Talk `silent` (spec 003) apply as for a single message.
- A group's deliveries are claimed, sent and recorded together: shared outcome, `sent_at` and retry time
  from the group's highest attempt; each is dead after its own `MaxAttempts`, as 012-AC7.
- README: delivery section notes the grouping.

Phase 2, gedour link:

- Telegram, Nextcloud Talk and email channel config gets `gedour_link` (boolean, default `true` when absent).
- When it is on and `GEDOUR_PUBLIC_URL` is set, a single-event message ends with a link "Open in
  gedour" to `<public URL>/inbox?event=<event id>` (the 010-AC13 deep link); a grouped message (phase 1)
  or a digest (012) ends with one "Open the inbox" link to `<public URL>/inbox`.
- Without `GEDOUR_PUBLIC_URL`, or with `gedour_link: false`, messages are as before.
- An email digest already ends with `Inbox: <url>`: that line is its link, never doubled.
- The link counts toward the channel's length cap: a cut message keeps it and the "+N more" line does
  not repeat it.
- Web Push is unchanged: a click already opens the entry (010-AC13); a grouped notification opens the
  inbox.
- API doc, OpenAPI, channel form checkbox "Link to gedour" (checked by default), "Send test" shows it,
  README.

## Out of scope

- Webhook and script channels: their payload is one event (as 012).
- Grouping across polls (a time window): it would delay every immediate message.
- Grouping across watches.
- Filtering grouped deliveries on inbox read state (012 keeps that to digests).
- A setting to turn grouping off.
- A per-event gedour link inside grouped messages and digests.

## Acceptance criteria

- [ ] **AC1** Given a watch whose poll yields 2 events matched by an immediate rule on a Telegram
  channel, the channel receives one message listing both, oldest first; both deliveries end `sent` with
  the same `sent_at`.
- [ ] **AC2** Same as AC1 on Nextcloud Talk, email and Web Push: one message (one notification per
  device for Web Push) per channel.
- [ ] **AC3** A poll yielding 1 event sends the same message as before this spec (existing single-message
  tests unchanged).
- [ ] **AC4** Events of the same watch from two different polls are sent as separate messages.
- [ ] **AC5** Events of two watches routed in the same pass are sent as separate messages.
- [ ] **AC6** On a webhook or script channel, each event of the poll is sent alone.
- [ ] **AC7** A high priority event and a normal one from the same poll on the same channel are sent as
  two messages; two high priority events from one poll are grouped.
- [ ] **AC8** A failed grouped send retries the whole group with the existing backoff; each delivery is
  `dead` after its own `MaxAttempts`.
- [ ] **AC9** A group whose events are read in the inbox before the send is still sent in full.
- [ ] **AC10** A delivery held by another claim (lease) keeps its group from going out in part: the
  group is claimed whole or not at all.
- [ ] **AC11** A grouped message longer than the channel cap is cut on a whole line and ends with
  "+N more" and the inbox link (012-AC8 behaviour).
- [ ] **AC12** Given `GEDOUR_PUBLIC_URL` set and a Telegram, Talk or email channel without `gedour_link`, a
  single-event message ends with a link to `<public URL>/inbox?event=<id>`.
- [ ] **AC13** A grouped message and a digest on such a channel end with one link to
  `<public URL>/inbox`, never one per event.
- [ ] **AC14** With `gedour_link: false`, or without `GEDOUR_PUBLIC_URL`, the message has no gedour
  link and is byte-identical to the message before this spec.
- [ ] **AC15** A channel saved with a non-boolean `gedour_link`, or `gedour_link` on another channel
  type, is a 400.
- [ ] **AC16** A message over the channel cap with the link on is cut on a whole line, ends with
  "+N more" and holds the inbox link once, within the cap.
- [ ] **AC17** The channel form shows "Link to gedour" checked for a new Telegram, Talk or email channel,
  saves the unchecked state as `gedour_link: false`, and reloads it unchecked.
- [ ] **AC18** Web Push: a grouped notification opens `/inbox`; a single one still opens
  `/inbox?event=<id>`.

## Phases

### Phase 1 — Grouped claim and send
- Work: claim of single deliveries leases, with each due row, every due unleased row sharing its
  channel, watch, `observed_at` and rule priority, under one token, on groupable channel types only;
  a group of 2+ goes through the digest send path without the read filter and with the "new events"
  header; README delivery section.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6, AC7, AC8, AC9, AC10, AC11
- **Data model impact**: index `events_watch_observed_idx` on `events (watch_id, observed_at)` (migration 00027,
  built concurrently), so the claim finds a poll's events without scanning `events`
- **DoD**: `make test` and `make coverage` green; `go test ./pkg/route -run 'Group'` covers every AC;
  `make lint` and `pre-commit run --all-files` green.

### Phase 2 — gedour link
- Work: `gedour_link` in the Telegram, Talk and email config (validation, default on); single, grouped and
  digest renderers add the link when on and a public URL is set; OpenAPI doc string (`make openapi`);
  channel form checkbox; README.
- **Covers**: AC12, AC13, AC14, AC15, AC16, AC17, AC18
- **Data model impact**: none (channel config is JSON; absent means on)
- **DoD**: `make test`, `make coverage`, `make lint`, `pre-commit run --all-files` green;
  `TestOpenAPIDrift` and `pnpm check:api` green; ChannelsPage vitest covers AC17.

## Data model impact (summary)

- `events_watch_observed_idx` on `events (watch_id, observed_at)` (phase 1, migration 00027): the claim
  looks up a poll's other events per due delivery; without it a backlog of thousands of due rows makes
  each claim seconds long.
- No column: `events.observed_at` already identifies the poll, `deliveries.rule_id` gives the priority,
  and `gedour_link` lives in the channel's JSON config.

## Open questions

- [ ] None blocking.

## Decisions

- Group on the poll, not a time window: no added latency; the reported case was one poll.
- High priority events stay apart from normal ones: a high event keeps its own loud message (005).
- No read filter on grouped deliveries: they leave at once, as 012 decided for immediate ones.
- Deliveries without a rule (rule test sends, 013) are never grouped: they are one deliberate event.
- Waiting for the whole poll to be routed beats sending in parts: the wait is one router pass, and a
  split poll would ping twice.
- gedour link on Telegram, Talk and email (babs, 2026-11-12); Web Push already opens the entry.
- Grouped messages and digests link to the inbox once, at the end (babs, 2026-11-12).
- `gedour_link` absent means on, so existing channels get the link without a migration.

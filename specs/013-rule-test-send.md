# 013 — Send a real event through a rule

**Status**: shipped
**Requested by**: babs
**Date**: 2026-11-10

## Problem

The rule form's preview only says whether the filter matches the last 20 events. A channel's "Send test"
sends a synthetic event (`gedour.test`, watch "gedour", no rule). Nothing shows the real message a rule
produces: a script channel that renders a Teams Adaptive Card, a Telegram message with release notes, a
high priority Web Push. The only way to see it today is to wait for the next upstream event.

## Solution

In the rule form's preview, each event row gets a "Live test" button. It sends that event through the
saved rule now: every channel of the rule receives the message a real routing would produce (same
event, watch, rule name and priority), marked as a test so nobody mistakes it for a real event. The form
then shows, per channel, whether it was sent, the error, and a script's log lines.

## Scope

- `POST /api/v1/rules/{id}/test` with body `{event_id}`.
- The event must be readable by the caller and inside the rule's scope (its watch, or any of the
  caller's watches for an all-watches rule). Otherwise 404.
- The delivery is built as the deliverer builds it (`Deliverer.load`): full payload, watch, rule
  `{id, name, priority}`.
- Test marker: the summary is prefixed with `[test] ` on every channel type (so in titles, Web Push and
  the script's `event.summary`); webhook bodies and script inputs carry `"test": true`, a field absent
  from real deliveries.
- Sent synchronously to each channel of the rule, one after the other, in the rule's channel order.
- Response: one item per channel `{channel_id, channel_name, ok, error, logs, accepted}`, same field
  meanings as `test-channel`.
- Rate limit: one test per `TestInterval` (10 s) per user, shared with channel tests. Audit action
  `rule.tested` (target the rule, event id in `after`).
- Script channels: a sandbox is reserved per run; none free → that channel's item fails with the busy
  error, the others are still sent.
- UI: "Live test" button on each preview row, edit page only, titled "Sends this event to the rule's
  channels now"; results shown in a row under the tested event.
- OpenAPI, web API client, README (rules section, webhook and script payload `test` field).

## Out of scope

- Testing an unsaved rule or unsaved form edits.
- A dry run that renders the message without sending it.
- MCP tool for this endpoint.
- Digest rendering: a digest rule's test sends the single-event message, as a high priority event would.

## Acceptance criteria

- [x] **AC1** Given a saved rule with channels A and B and an event in its scope, when the owner calls
  `POST /rules/{id}/test {event_id}`, then A and B each receive the message a real routing of that event
  by that rule sends, with its summary prefixed `[test] `, and the response has one `ok: true` item per
  channel.
- [x] **AC2** A script channel receives `script.InputOf` of the real delivery: real event payload, watch,
  `rule: {id, name}`, `event.summary` prefixed `[test] ` and `test: true`; the response item carries the
  run's log lines.
- [x] **AC3** A Web Push channel on a `high` priority rule is sent with the urgency a real high priority
  delivery gets; the item has `accepted`.
- [x] **AC4** A digest rule's test sends immediately, one single-event message per channel; no digest
  is queued.
- [x] **AC5** No row is written to `deliveries` and no inbox item changes; one `rule.tested` audit row
  is written.
- [x] **AC6** An event outside the rule's scope, unreadable by the caller, or nonexistent → 404. A rule
  the caller does not own → 404.
- [x] **AC7** A second rule or channel test within 10 s of the previous one → 429 with `Retry-After`.
- [x] **AC8** One channel failing (send error, busy sandbox) does not stop the others; its item has
  `ok: false` and `error`.
- [x] **AC9** A rule with no channel → 200 with an empty list; the UI says the rule has no channel.
- [x] **AC10** On `/rules/:id`, each preview row has a "Live test" button; clicking it shows the
  per-channel results; a 429 shows the retry delay. On `/rules/new` there is no button.
- [x] **AC11** A webhook channel's body has `"test": true`; a real delivery's body and script input have
  no `test` field, and their summary has no prefix.

## Phases

### Phase 1 — Endpoint
- Work: route, delivery loading shared with `Deliverer.load`, `Test` flag on `notify.Delivery` read by
  `notify.Format`, `WebhookBody` and `script.InputOf`, per-channel send through `send` / `pushSend`, rate
  limit and audit shared with `testChannel`, OpenAPI regenerated (`make openapi`).
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6, AC7, AC8, AC9, AC11
- **Data model impact**: none
- **DoD**: `go test ./pkg/route/...` green with tests for every AC above; `make lint` green.

### Phase 2 — UI
- Work: "Live test" button in `PreviewTable` on the edit page, results list, 429 handling, README.
- **Covers**: AC9, AC10
- **Data model impact**: none
- **DoD**: vitest covers the button, a success, a partial failure, and a 429; e2e on a lab build
  sends a real event to a script channel and the result shows in the form.

## Data model impact (summary)

None. One new audit action, `rule.tested`.

## Open questions

- (none)

## Decisions

- The test uses the saved rule, not the form state: the message must match what routing sends, and
  routing reads the saved rule. Unsaved edits → save first.
- The filter is not checked: a non-matching event can be sent, to see how it renders.
- The message is a real one with a `[test] ` summary prefix: on a shared channel an unmarked message reads
  as a real release. The prefix goes through `notify.Format`, so every channel type and existing
  scripts show it without changes; `test: true` is the machine-readable form.
- The rate limit is shared with channel tests: sending a rule test to N channels counts as one test.
- Channels are sent one after the other: at most 20 channels per rule, and the
  per-channel timeout bounds the request.
- Nextcloud Talk's hold (`holdTalk`) and the digest schedule are bypassed: the user asked for a send now.

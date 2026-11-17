# 018 — Digest weekdays

**Status**: shipped
**Requested by**: babs
**Date**: 2026-11-17

## Problem

A digest rule sends at the same times every day. A user who wants a 09:00 digest on workdays and one at
18:00 on weekends needs two rules, and a digest that should skip the weekend cannot be set at all.

## Solution

Each digest time carries the weekdays it runs on. The rule form shows seven day toggles next to each time,
all on by default. The rules list names the days when a time does not run every day.

## Scope

- Rule API: `schedule`, a list of `{time, days}`, replaces `at` in the rule output, create and update
  bodies. `time` is `HH:MM`; `days` is a list of `mon`..`sun`, normalized to Monday-first order without
  duplicates; omitted or `null` `days` means every day. `tz` is unchanged.
- Validation: 1 to 24 entries, a valid `time`, at least one known day when `days` is given, entries with the
  same `time` merged (union of their days), entries sorted by time.
- Scheduling: the next digest instant is the first `(day, time)` strictly after now, in `tz`, with the
  existing DST handling (012). A time runs on a day of `tz`'s calendar.
- A days change reschedules the rule's waiting deliveries, as a time or zone change does (012-AC18).
- Migration: every existing digest rule gets every day on each of its times. Sends do not move.
- UI: `DeliveryFields` gets a row of seven `aria-pressed` toggles per time (Mon..Sun); the rules list
  summary reads `Digest at 09:00 Mon–Fri, 18:00 Sat, Sun (Europe/Paris)`; a time on every day shows no days.
- MCP `create_rule` / `update_rule` follow from the huma schema; `make openapi` regenerated.
- README digest bullet and spec 012's `at` wording updated; `go test ./pkg/server -run TestDocs` green.

## Out of scope

- Other schedule shapes: every N minutes, monthly, dates, holidays.
- A per-time time zone.
- Accepting the old `at` field alongside `schedule` (the bundled UI is the only known client).
- Channel-level schedules (removed by 012 phase 4).

## Acceptance criteria

- [x] **AC1** A rule created with `schedule: [{"time":"09:00","days":["mon","tue","wed","thu","fri"]}]` is
  returned with that schedule; `GET` and `list_rules` show it.
- [x] **AC2** An entry without `days` (or `days: null`) is stored and returned with all seven days.
- [x] **AC3** 400 with a `body.schedule[i]...` location on: no entry, more than 24 distinct times, a bad
  `time`, `days: []`, an unknown day.
- [x] **AC4** Two entries with the same `time` are merged into one with the union of their days; days come
  back Monday-first, without duplicates; entries come back sorted by time.
- [x] **AC5** A body with `at` is rejected (unknown field), on create and update.
- [x] **AC6** Given `09:00 mon–fri` in `Europe/Paris`, a delivery queued on Friday 10:00 is due Monday
  09:00 Paris; queued Thursday 08:00 it is due Thursday 09:00.
- [x] **AC7** Given `09:00 sat` only, the next instant from any point of the week is the coming Saturday
  09:00, including from Saturday 09:00 itself (next Saturday).
- [x] **AC8** A time on a day whose wall time is skipped or repeated by DST follows 012's rule on that day.
- [x] **AC9** Changing only the days of a digest rule moves its waiting, unleased deliveries to the new
  next instant.
- [x] **AC10** After the migration, each existing digest rule has all seven days on each former time and
  its waiting deliveries keep their `next_attempt_at`.
- [x] **AC11** The rule form shows seven day toggles per time, all pressed for a new time; unpressing the
  last one shows "at least one day" and blocks the save; the saved rule matches the toggles.
- [x] **AC12** The rules list shows `Digest at 09:00 Mon–Fri, 18:00 Sat, Sun (Europe/Paris)` for that
  schedule and `Digest at 09:00 (UTC)` for an every-day time.
- [x] **AC13** README and spec 012 describe `schedule` instead of `at`; `TestDocs` and `TestOpenAPIDrift` green.

## Phases

### Phase 1 — API, scheduling, migration
- Work: `schedule` type and `checkSchedule` on `{time, days}`; `nextAt` over up to 8 days filtered by
  weekday; reschedule on days change; rule input/output and SQL; migration; `make openapi`; README and
  spec 012 text.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6, AC7, AC8, AC9, AC10, AC13
- **Data model impact**: `rules.digest_days smallint[]`, one weekday bitmask per `digest_at` entry (bit 0
  Monday), backfilled to 127 for existing digest rules, NULL for immediate ones.
- **DoD**: `make test` and `make coverage` green (Go not below develop's); unit tests for AC4, AC6-AC8;
  route tests for AC1-AC3, AC5, AC9; migration test for AC10 on a populated DB.

### Phase 2 — UI
- Work: day toggles in `DeliveryFields`, `digestErrors` for days, `digestSummary` with day ranges,
  `RuleFormPage` state on `schedule`; e2e for the digest form updated.
- **Covers**: AC11, AC12
- **Data model impact**: none
- **DoD**: `make lint`, `make coverage` (web not below develop's) green; vitest for AC11, AC12; full e2e
  green through the bounded runner (`scripts/e2e.sh --workers=4`).

## Data model impact (summary)

- `rules.digest_days smallint[]`, same length as `digest_at` when the rule is a digest.

## Open questions

- [ ] none

## Decisions

- Structured `schedule` replaces `at` (user choice); no compatibility shim, as the UI ships in the same binary.
- Storage is a bitmask array next to `digest_at`, not a jsonb rewrite: an additive migration, existing
  queries on `digest_at` keep working.
- Days are `mon`..`sun`, Monday first (ISO 8601); the UI shows them in that order.
- Same `time` twice merges days instead of a 400: it matches today's silent dedupe of repeated times. The
  UI still flags a repeated time.
- Omitted `days` means every day: a client that only knows times keeps a daily digest.

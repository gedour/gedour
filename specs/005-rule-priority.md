# 005 — High priority rules

**Status**: approved
**Requested by**: babs
**Date**: 2026-11-05

## Problem

All inbox items look alike. A release fixing a CVE sits among routine tags, and the only way to treat it
differently today is to route it to a louder channel. Nothing in the inbox says what matters first.

## Solution

A rule can be marked high priority. An event taken by such a rule is marked high in the inbox of that rule's
owner: it shows a badge, and a "High priority" view lists only those. Classification stays in the user's
hands, with the filters they already write, for example `len(event.payload.cves) > 0` (spec 004).

## Scope

- `priority` on rules: `normal` (default, also for existing rules) or `high`; API and rule form.
- `priority` on inbox items, set when the event is routed. Several matching rules of one user still make one
  item, and it is high if any of them is high.
- Inbox: badge on high items, in the list and the detail panel; a filter for high items; the list order stays
  newest first.
- API: `priority` on inbox items and a `priority` query filter on the inbox list.

## Out of scope

- More than two levels, a score, or CVSS lookup.
- Changing channel messages or delivery: a loud channel for high rules and a Silent one for the rest already
  gives the notification side (spec 003).
- Re-classifying existing items when a rule changes: a priority is set once, at routing.
- A separate unread counter for high items.
- Other users' inboxes: priority belongs to the rule's owner, each user's rules decide for their own inbox.

## Acceptance criteria

- [ ] **AC1** A rule is created and edited with a priority through the API and the form; omitted means `normal`,
  an unknown value is a 400, existing rules read `normal`.
- [ ] **AC2** An event matched by a high rule creates an inbox item with priority `high`; one matched only by
  normal rules is `normal`.
- [ ] **AC3** An event matched by a normal and a high rule of the same user makes one item, `high`, whatever the
  order the rules run in.
- [ ] **AC4** Two users watching the same target with different rules see their own priority for the same event.
- [ ] **AC5** Inbox items show a badge when high, conveyed by text and not by color alone; the high priority
  filter lists only those, with working pagination.
- [ ] **AC6** Editing a rule's priority leaves existing items unchanged and applies to the next events.
- [ ] **AC7** The rule form control and the badge pass the a11y (light and dark), 375 px, keyboard and CSP checks.
- [ ] **AC8** A rule's priority change is in the audit log.

## Phases

### Phase 1 — Priority on rules and items
- Work: migration, rule API and form, router, inbox API and UI, README, OpenAPI.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6, AC7, AC8
- **Data model impact**: new migration: `rules.priority` and `inbox_items.priority`, text with a check constraint,
  default `normal`; an index for listing a user's high items.
- **DoD**: `make coverage` and `make e2e` green; an e2e high rule on a fixture release shows the badge and the
  filter in the inbox.

## Data model impact (summary)

- `rules.priority`, `inbox_items.priority`: `normal` or `high`, default `normal`, existing rows `normal`.
- Partial index on `inbox_items` for high items.

## Open questions

- [ ] None.

## Decisions

- Priority sits on the rule, not on a separate classifier: filters already express the condition.
- High items are not pinned to the top: the list stays chronological, with the filter to isolate them. Pinning
  would let old unread items hide new ones. Overturnable.
- A priority is set once, so an item never changes level after the user has seen it.
- Spec 005 ships after 004: it works without it, but the CVE example needs `payload.cves`.

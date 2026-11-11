# 014 — Rules on several watches

**Status**: in progress
**Requested by**: babs
**Date**: 2026-11-10

## Problem

A rule covers one watch or all of the owner's watches. To route three repositories the same way, the user
writes three identical rules, or one all-watches rule with a filter `watch.id in ["<uuid>", ...]`. That
filter works but stays a workaround:

- The form and the rules list show "All watches", so nobody can tell what the rule covers without reading
  UUIDs in the filter.
- The UUIDs are copied by hand from the watch URLs. `watch.name in [...]` avoids that but breaks on a rename.
- The preview runs on the last 20 events of every watch: a quiet watch in the list is drowned by busy ones,
  and the preview shows 20 events the rule never sees.
- The ids eat the filter's budget: 2000 characters and 50 array items, so about 49 watches at most, fewer
  once the real condition is added.
- A deleted watch's id stays in the filter for good.

## Solution

The rule form offers "All watches" or a list of the user's watches to pick from. The picker is built for
a user with thousands of watches: a server-side search on name and target, a kind filter, "select all matching", a "selected only"
view, and the picked watches shown as removable chips above the list. A rule routes the events of the
picked watches only. The rules list names them. The preview runs on the last 20 events of the picked
watches. Deleting a watch takes it out of the rules that list it; a rule left with no watch is deleted, as a
one-watch rule is today.

## Scope

- Rule scope: all watches, or a set of 1 to 500 of the owner's watches (500 is the `listWatches` page
  maximum, so "select all matching" fits one call). REST has no cap on owned watches.
- API: `watch_ids` on create, update and preview: `[]`, or absent on create and preview, means all
  watches; an update without `watch_ids` keeps the list. A duplicate counts once. On create and update, an id the caller does not own is a 400 naming it, except on update an id the
  rule already lists (a transferred watch stays listed, AC8). On preview, an id the caller does not own adds
  no events. Rule output gains `watch_ids` (sorted) and
  `watches` (`id`, `name`, `name` null while the owner does not own that watch).
- `watch_id` stays as a deprecated alias: accepted on input (one id, or empty for all watches), a 400 when
  sent with `watch_ids`; in the output it holds the watch only for a one-watch rule and is null otherwise,
  `watch_name` likewise. OpenAPI marks both deprecated.
- Router: an event is evaluated against the owner's all-watches rules and the rules that list its watch.
- Watch delete and account purge: the watch leaves every rule's list; a rule left with no watch is deleted.
  The purge preview lists the other users' rules it deletes, as today.
- Transfer: a rule keeps a transferred watch in its list and routes nothing from it until it comes back,
  as today.
- MCP `create_rule`, `update_rule`, `get_rule`, `list_rules` follow the API schema.
- Web watch picker in the rule form, reusing the Watches page parts (search box, kind select, highlighted
  term, `KindIcon`, `PausedBadge`):
  - Two choices: "All watches" or "Selected watches".
  - Search on name and target and kind filter, server side through `listWatches` `q` and `kind`
    (debounced), the term highlighted.
  - The list shows the first 100 matches; past that it says "Showing the first 100 matches: refine the search".
    Rows: checkbox, kind icon, name, target, paused badge; the list scrolls in a fixed-height box, the
    form around it does not.
  - "Select all matching" picks every match of the current search and kind (one `listWatches` call,
    limit 500), not only the 100 shown, and refuses with a message when the call returns a full page of 500 (there may be more) or the total would pass 500;
    "Clear matching" unpicks them. A "Selected only" toggle lists the picks from the form's state, no
    request. A "N selected" count.
  - Chips of the picked watches above the list, each removable; past 10 they fold into "+N more", which
    unfolds; a watch the user no longer owns shows as "A watch you no longer own", still removable.
  - Empty states: no watch at all (link to create one), no watch matching the search.
- Rules list: "All watches", the watch name, or the first two names and "+N", a button opening the full
  list (works by tap and keyboard).
- Preview on the picked watches, refreshed when the selection changes.
- OpenAPI regenerated (`make openapi`), README.

## Out of scope

- Selecting watches by kind, name pattern or tag ("every OCI watch"): a filter on `watch.kind` or
  `watch.name` already does it.
- Excluding watches from an all-watches rule.
- Showing on a watch's page the rules that cover it.
- Removing the `watch_id` alias: a later major version.

## Acceptance criteria

- [ ] **AC1** Given watches A, B, C of the user, a rule saved with `watch_ids: [A, B]` routes the events of
  A and B and not those of C.
- [ ] **AC2** A rule saved with `watch_ids` absent or `[]` routes every watch of the owner, including one
  created after the rule.
- [ ] **AC3** `watch_ids` naming a watch the caller does not own, or a non-UUID, is a 400 located at
  `body.watch_ids[i]`; more than 500 items is a 400; a duplicate id is stored once.
- [ ] **AC4** `watch_id: A` on create or update stores the scope `[A]`; `watch_id: ""` stores all watches;
  `watch_id` and `watch_ids` together is a 400.
- [ ] **AC5** A rule's output holds `watch_ids` sorted and `watches` with each name; `watch_id` and
  `watch_name` are set for a one-watch rule and null for an all-watches or a multi-watch rule.
- [ ] **AC6** Update with `watch_ids` replaces the list; an update without `watch_ids` or `watch_id` keeps it.
- [ ] **AC7** Deleting watch A takes it out of a rule on `[A, B]`, which keeps routing B; a rule on `[A]`
  alone is deleted with the watch.
- [ ] **AC8** After watch A is transferred to another account, the old owner's rule on `[A, B]` routes B
  only, shows A with a null name, and routes A again after a transfer back.
- [ ] **AC9** Purging an account deletes the other users' rules whose every watch belonged to it, and lists
  them in the purge preview; a rule that keeps another watch loses the purged one and stays.
- [ ] **AC10** The preview with `watch_ids: [A, B]` evaluates the last 20 events of A and B only;
  `watch_id` works there as on rules.
- [ ] **AC11** The migration keeps every existing rule's scope: a one-watch rule becomes a list of that
  watch, a null `watch_id` becomes all watches; its down step restores `watch_id`, and fails when a rule has
  several watches, naming the rules to edit.
- [ ] **AC12** A watch delete, a purge, a rule edit and the router racing each other neither deadlock nor
  leave a rule on a deleted watch (the existing race tests, extended to multi-watch rules).
- [ ] **AC13** The rule form offers "All watches" or "Selected watches"; saving "Selected watches" with
  none picked is blocked in the form with a message on the field; editing a rule opens with its scope and
  its picked watches checked.
- [ ] **AC14** MCP `create_rule` with `watch_ids` creates a multi-watch rule; `get_rule` returns its
  `watches`.
- [ ] **AC15** With 2000 watches, the picker shows the first 100 and says there are more; typing in the
  search narrows the list on name and target with the term highlighted, the kind filter narrows it further,
  and "no watch matches" shows when nothing does.
- [ ] **AC16** "Select all matching" picks every match of the search and kind, including those past the 100
  shown, and keeps earlier picks; past 500 picks in total it refuses with a message and picks nothing;
  "Clear matching" unpicks only the matches; "Selected only" lists the picks; the count matches them.
- [ ] **AC17** The picked watches show as chips, each removable; past 10 they fold into "+N more"; a picked
  watch the user no longer owns shows as "A watch you no longer own" and can be removed.
- [ ] **AC18** The picker works with the keyboard alone (Tab to the search, the filters and each checkbox,
  Space toggles) and the rule form, picker open, passes the axe check of `web/e2e/a11y.spec.ts`.
- [ ] **AC19** The rules list shows "All watches", the watch name, or the first two names and "+N" with the
  full list behind "+N", opened by tap or keyboard.
- [ ] **AC20** The preview runs on the picked watches and reruns when the selection changes.

## Phases

### Phase 1 — Scope in the database and the router
- Work: migration 00026 (join table, explicit all-watches flag, backfill, drop `rules.watch_id`); router
  query on the new scope; watch delete and purge locking, emptied-rule delete, purge preview; rule
  read/create/update and preview on `watch_ids`, with the `watch_id` alias.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6, AC7, AC8, AC9, AC10, AC11, AC12, AC14
- **Data model impact**: new `rule_watches (rule_id, watch_id)`, PK on both, `ON DELETE CASCADE` on both,
  index on `watch_id`; new `rules.all_watches boolean NOT NULL`; `rules.watch_id` and `rules_watch_id_idx`
  dropped after the backfill.
- **DoD**: migration up, down, up on a populated DB (`TestMigrations` style) green, down refused with a
  multi-watch rule; `go test -race ./pkg/route/... ./pkg/auth/... ./pkg/watch/... ./pkg/mcpserver/...
  ./pkg/server/...` green, with the race tests of AC12; `make openapi` leaves no diff after commit.

### Phase 2 — Web picker, list, docs
- Work: a `WatchPicker` component used by `RuleFormPage`, sharing the search and kind parts with
  `WatchesPage`; scope label in `RulesPage`; preview on the picked watches; README (routing, API, MCP
  sections).
- **Covers**: AC13, AC15, AC16, AC17, AC18, AC19, AC20
- **Data model impact**: none
- **DoD**: vitest for the picker (search, kind, select/clear matching, the 500 refusal, selected only, chips fold, lost watch)
  and the list label green; an e2e spec on a 2000-watch fixture, `keyboard.spec.ts` and `a11y.spec.ts`
  green through the bounded runner (`scripts/e2e.sh --workers=4`); `make coverage` not below develop's; screenshots of the
  picker (empty, searching 2000 watches, 30 picked) in light and dark shown at the merge.

## Data model impact (summary)

- New table `rule_watches (rule_id uuid REFERENCES rules ON DELETE CASCADE, watch_id uuid REFERENCES watches
  ON DELETE CASCADE, PRIMARY KEY (rule_id, watch_id))`, index on `watch_id`.
- `rules.all_watches boolean NOT NULL`: true routes every watch; false routes the listed ones.
- `rules.watch_id` and its index dropped, after the backfill.

## Open questions

- [ ] None.

## Decisions

- A real scope over the `watch.id in [...]` filter: the filter hides the scope from the form, the list and
  the preview, and caps it near 45 watches.
- `watch_id` kept as a deprecated alias (user's choice, 2026-11-10): v1 clients keep writing. A v1 client
  reading a multi-watch rule sees `watch_id: null` and may take it for all watches; documented in the
  OpenAPI deprecation note.
- An explicit `all_watches` flag rather than "no row means all": a cascade or a missed delete path that
  empties a rule's list then routes nothing instead of widening the rule to every watch.
- A rule left with no watch is deleted in the same transaction as the watch, as the cascade does today for
  a one-watch rule; no audit row of its own, as today.
- `[]` means all watches on the API, like today's empty `watch_id`; the form blocks an empty selection with
  "All watches" off, so a click cannot widen a rule.
- Max 500 watches per rule, the `listWatches` page maximum: "select all matching" stays one call, and the
  request body stays near 20 KB. A set past 500 is better written as all watches plus a filter on
  `watch.kind` or `watch.name`, which also follows new watches.
- Picker search and kind filter run server side: REST has no cap on owned watches, so the client cannot
  load them all. 100 rows shown, no list virtualization; the search narrows the rest.
- Picker built from the existing Watches page parts and native checkboxes, no new dependency (`radix-ui`
  already covers the tooltip).
- Purge deletes only the other users' rules left with no watch; a rule keeping another watch already
  routed nothing from the purged one since the transfer.
- The down migration refuses multi-watch rules rather than keeping one watch or widening to all.
- Spec 013 (rule test send) checks that an event is in the rule's scope; whichever lands second uses the
  other's scope check.
- An update accepts an id the rule already lists even when its owner no longer owns it: a round trip of
  `get_rule` then `update_rule`, or an edit in the form, keeps a transferred watch listed (found in phase 2
  review, 2026-11-11).

# 009 — Remove watch sharing

**Status**: in progress
**Requested by**: babs
**Date**: 2026-11-06

## Problem

Watch sharing (001, read-only and read-write shares) costs more than it gives. Every watch query joins
`watch_shares`, every route has two permission levels below owner, and the router, inbox, deliveries,
audit visibility, transfer, purge, disable, MCP and the watches page each carry a share case. Few users
share watches. The feature goes.

## Solution

A watch belongs to its owner and nobody else sees it. The share API, the share section of the watch
page, the "Shared by" badge and the ownership filter disappear. Another user's watch answers 404 on
every route, as an unknown one does today.

On upgrade, an install that has shares loses them. Each former sharee loses what the share gave them:
rules pinned to the shared watch, inbox items and channel deliveries for its events. The owner keeps
everything. Each dropped share leaves a system audit entry the owner can read.

A disabled account's watches stop collecting: without sharing, their events reach nobody.

## Scope

- Migration `00017`: delete the former sharees' data listed above, write one audit entry per share,
  drop `watch_shares` and its trigger function.
- API: the four `/api/v1/watches/{id}/shares` operations go. `Watch` loses `permission`, `owner_id`
  and `owner_name`; `GET /api/v1/watches` loses `shared`. Every watch route checks ownership only.
- Routing, inbox, deliveries, rules (scope, preview), script test events and "run all" use owned
  watches only.
- Admin: `shared_watches` (users list), `shares` (user's watches), `shares_received` and
  `shared_watches` (purge preview and purge audit), `dropped_share` (transfer) go, with their UI copy.
- Audit: share entries already written stay readable by the watch owner; the "Watch share" target
  filter stays for them.
- MCP: tool descriptions lose sharing; outputs follow the REST changes.
- UI: watch detail without the share section, watches page without the ownership filter and with
  every tile selectable, delete dialog and rule form without share wording.
- Worker: no claim on a watch whose owner is disabled; the due gauge leaves them out.
- README, AGENTS.md (lock order), `specs/README.md`, e2e seed and specs.

## Out of scope

- Teams, groups or any other way to give a second user access to a watch.
- Editing specs 001 to 008: they stay as history; `specs/README.md` notes what 009 removed.
- Transfer's rules: the old owner's rules on a transferred watch stay and route nothing, as today.
- Restoring deleted rows on rollback (see Rollback).

## Acceptance criteria

- [ ] **AC1** Given a database at version 16 where alice's watch W is shared with bob, bob has a rule
  pinned to W with a channel, an inbox item and a delivery for an event of W, and a rule on all
  watches, when migration 17 runs, then bob's pinned rule, its `rule_channels` row, the inbox item and
  the delivery are gone; bob's all-watches rule, W, its events and alice's rules, items and deliveries
  stay; `watch_shares` and `watch_shares_not_owner()` no longer exist.
- [ ] **AC2** Migration 17 writes one `watch_share.removed` audit row per share, actor null, target
  `<watch>:<user>`, `before` holding user ID, email and permission. Alice sees it in her audit view.
- [ ] **AC3** A rule on another user's watch that no share covers (left by a transfer) survives
  migration 17.
- [ ] **AC4** Migration 17's down recreates an empty `watch_shares` with its index, function and
  trigger; up after down succeeds.
- [ ] **AC5** Every method on `/api/v1/watches/{id}/shares` and `/api/v1/watches/{id}/shares/{user_id}`
  answers problem+json 404; `api/openapi.json` has no share operation or schema.
- [ ] **AC6** Bob gets 404 on every watch route for alice's watch (get, update, delete, run, states,
  events), never 403.
- [ ] **AC7** A watch in a response has no `permission`, `owner_id` or `owner_name`. `GET
  /api/v1/watches` lists the caller's watches only; a `shared` query parameter changes nothing.
- [ ] **AC8** A rule naming another user's watch is refused like an unknown watch; preview, inbox,
  deliveries, the router and script test events only reach the caller's own watches.
- [ ] **AC9** Admin responses carry no share field: users list, user's watches, purge preview, transfer
  result. A purge's audit `deleted` counts have no `shares_received`.
- [ ] **AC10** The watch detail page has no share section or "Shared by" badge. The watches page has no
  ownership filter and every tile is selectable. The delete dialog, the rule form and the admin
  disable, purge and transfer dialogs name no share.
- [ ] **AC11** MCP `list_watches` and `run_watch` descriptions name no share; `list_watches` output has
  no `permission`, `owner_id` or `owner_name`.
- [ ] **AC12** While alice is disabled, no worker claims her due watches and the due gauge leaves them
  out. After re-enable, the next claim takes them.
- [ ] **AC13** No file under `pkg cmd web/src web/e2e`, nor `README.md` or `AGENTS.md`, matches
  `watch_shares|sharee|shared_with|addShare|SharedBadge|with_me|levelWrite|shares_received|SharedWatch|dropped_share`
  (case-insensitive), except migrations `00003` and `00017`, the migration test and the guard test
  `pkg/server/noshare_test.go`, whose `shareNames` holds the pattern and enforces it.
- [ ] **AC14** README, AGENTS.md and `specs/README.md` describe no sharing; AGENTS.md's lock order has
  no `addShare` and no share wait. `grep -niE 'shar(e|es|ing)\b|shared with' README.md AGENTS.md`
  matches only PostgreSQL lock modes (`FOR SHARE`, `KEY SHARE`); in `specs/README.md` only the
  001 and 006 rows name sharing, to say 009 removed it.

## Phases

### Phase 1 — Remove sharing
- Work: migration `00017` and its test; `pkg/watch` (shares, levels, `ReadableSQL`, list filter,
  run-all, sharee checks); `pkg/route`, `pkg/audit`, `pkg/auth` (admin, purge, transfer); MCP
  descriptions; `make openapi`; web client, pages and components; Go, web and e2e tests (share e2e
  removed, the audit e2e reworked without a share).
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6, AC7, AC8, AC9, AC10, AC11, AC13
- **Data model impact**: drops `watch_shares` and `watch_shares_not_owner()`; deletes former sharees'
  pinned rules, inbox items and deliveries; inserts audit rows.
- **DoD**: `make lint`, `make test`, `make coverage` and `make e2e` green; the AC13 guard test, over
  `pkg cmd web/src web/e2e`, green.

### Phase 2 — No collection for a disabled owner
- Work: worker claim and due gauge skip watches of disabled owners; enable moves a due time gone stale to
  now; the admin disable dialog says the account's watches stop collecting; tests.
- **Covers**: AC12
- **Data model impact**: none
- **DoD**: `go test ./pkg/worker/... ./pkg/auth/...` green, with a test that disables an owner, finds
  no claim, re-enables and finds the claim; a vitest on the disable dialog copy; `make lint` green.

### Phase 3 — Docs
- Work: README (API, MCP, admin, audit, routing sections), AGENTS.md lock order, `specs/README.md` row
  for 009 and notes on 001 and 006; the AC13 guard test also checks `README.md` and `AGENTS.md`.
- **Covers**: AC13, AC14
- **Data model impact**: none
- **DoD**: the AC13 guard test green with the docs included; the AC14 grep matches only what AC14
  allows; `pre-commit run --all-files` green.

## Data model impact (summary)

- Migration `00017_drop_watch_shares.sql`: `DELETE` from `rules` (pinned to a watch shared with their
  owner), then `deliveries` and `inbox_items` (former sharee on the shared watch's events);
  `INSERT INTO audit_log` per share; `DROP TABLE watch_shares`; `DROP FUNCTION watch_shares_not_owner()`.
- The migration first locks `watch_shares` (ACCESS EXCLUSIVE), so an old pod cannot write a former
  sharee's row between the deletes and the drop. The drop comes last and takes ACCESS EXCLUSIVE on
  `watches` and `users` (FK triggers). Each lock wait stops at `lock_timeout` 5 s, and the pod restart
  retries.

## Rollout and rollback

- Rolling update: while old pods run next to a migrated database, their watch, inbox and routing
  queries fail with 500 because `watch_shares` is gone. Events are not lost: a failed routing
  transaction leaves them unrouted, and the new workers route them. A delivery loaded by an old worker
  in that window may spend one retry.
- Rollback: an older image starts on a version-17 database, since goose ignores versions it does not
  know, but it fails on every watch query. Run migration 17's down section by hand (`psql`), delete
  version 17 from `goose_db_version`, then deploy the older image. Shares come back empty.
- The deleted rules, inbox items and deliveries come back only from a database backup taken before
  the upgrade. Share rows can be rebuilt by hand from the audit log: each `watch_share.removed` row
  holds the user and permission.

## Open questions

- [ ] None.

## Decisions

- Former sharees' pinned rules, inbox items and deliveries are deleted, not left dormant: babs's call.
  Selection goes through the share rows, so transfer leftovers stay.
- One migration drops the table, no two-step release: the rollout window above is accepted.
- `owner_id` and `owner_name` go with `permission`: on an owner-only API they always name the caller.
- Dropped shares get system audit rows; deleted rules, items and deliveries do not. The share row is
  what an owner can act on, and what a manual restore needs.
- Collection stops for a disabled owner (006's "keep collecting for shares" no longer holds). On
  re-enable a due time gone stale moves to now and the next run reports what differs from
  their last stored state; a change reverted meanwhile, or beyond a collector's window, is not reported.
- Enable and transfer move a due time gone stale to now, so a re-enabled or rescued watch does not
  jump ahead of other owners' due work; a future one (a rate-limit hold) stays, and so does a leased
  one, which the worker would read as a run-now.
- `ReadableSQL` keeps its name and becomes the ownership check: its callers stay as they are.

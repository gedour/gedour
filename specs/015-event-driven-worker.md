# 015 — Event-driven worker loops

**Status**: shipped
**Requested by**: babs
**Date**: 2026-11-10

## Problem

The three loops of the worker role (watch runs, routing, deliveries) wake on a fixed `GEDOUR_WORKER_POLL`
ticker, 2 s by default. An idle worker process still sends about five queries per tick (due count, watch
claim, route batch, single and digest delivery claims), about 2.5 queries per second for the two lab
replicas, almost all of them empty. Lowering the rate with a longer poll delays everything a user
triggers: a run-now, a new watch, a retry or a fresh event waits up to one poll before anything happens.

## Solution

Each loop sleeps until it has work: the time the next watch or delivery falls due, or a signal that the
database changed. Writes to the scheduling data emit a PostgreSQL notification that wakes the loops of
every worker replica at once; each wake adds a small random delay so replicas do not claim in lockstep. A
slow safety poll remains for notifications lost while a connection is down. An idle worker sends about
one query per loop per minute, and a run-now starts in well under a second.

## Scope

- One notification channel, `gedour_work`, with the table name as payload (`watches`, `events`,
  `deliveries`). Statement-level triggers emit it, so a bulk statement sends one notification and every
  writer is covered (web, MCP, worker, future code paths):
  - `watches`: `AFTER INSERT OR UPDATE OF next_run_at, paused`.
  - `events`: `AFTER INSERT`.
  - `deliveries`: `AFTER INSERT OR UPDATE OF next_attempt_at, status`.
- A notification is sent at commit, so a wake never sees uncommitted rows. A rolled-back write sends
  nothing.
- One dedicated LISTEN connection per worker process, hijacked out of the pool like the web hub's.
  Dropped, it reconnects with backoff (1 s doubling to 30 s) and wakes every loop once it listens again:
  anything sent meanwhile is lost.
- Each loop has a wake signal (buffered, size 1): a burst of notifications collapses into one wake.
- After a pass that claims nothing, a loop sleeps until the earliest of:
  - its next deadline, read with the same predicates as its claim (watches: `min(next_run_at)` of
    unpaused, unleased watches of enabled owners; deliveries: `min(next_attempt_at)` of pending or
    failed, unleased deliveries; router: none, it only has notifications);
  - a wake signal;
  - the safety poll, `GEDOUR_WORKER_POLL`.
- Every wake, of any cause, waits a random 0 to 250 ms before claiming.
- Two consecutive passes of one loop are at least 1 s apart (`minPass`). A deadline already past but
  not claimable (a row locked by an edit) never makes a loop spin.
- A pass that fails (claim or route error) waits `minPass` doubling to the safety poll before the next
  one, as the error log already shows.
- `GEDOUR_WORKER_POLL` becomes the safety poll; its default goes from `2s` to `60s`.
- The Talk "channel busy" hold re-queues with a fixed 1 s delay instead of `poll/2`.
- `gedour_watches_due` is read at the end of each pass of the watch loop: the due watches it left unclaimed (no
  free slot, or row locked by another transaction). It is at most one safety poll old.
- New counter `gedour_worker_wakeups_total{loop, reason}`: `loop` is `watches`, `router` or
  `deliveries`; `reason` is `deadline`, `notify`, `poll` or `retry` (the pass after a failed one).
- README: the `GEDOUR_WORKER_POLL` row, the Talk throttle paragraph ("one message per
  `GEDOUR_WORKER_POLL`"), the metrics list, and the architecture section on the worker loops.

## Out of scope

- The web role's inbox stream (LISTEN on the inbox channel): unchanged.
- Waking a single replica instead of all of them. SKIP LOCKED already keeps claims exclusive; the extra
  wakes cost one deadline query each.
- Timely pickup of a crashed replica's leases or of claims released at shutdown: they are taken at the
  next safety poll, up to 60 s after the lease expires or the claim is released.
- Changes to the claim queries, leases, retry and backoff policies, or concurrency.
- A Talk channel edited during its 401 pause: its held deliveries keep their `next_attempt_at`, as today.

## Acceptance criteria

- [x] **AC1** Given an idle worker process (no due watch, no unrouted event, no due delivery), over
  120 s the three loops together run at most 9 passes (`gedour_worker_wakeups_total` delta), against
  about 180 today.
- [x] **AC2** Given an idle worker sleeping on the safety poll, when the web API runs a watch now, the
  watch is claimed within 1 s.
- [x] **AC3** Given a watch whose `next_run_at` is 3 s ahead and no other activity, the worker claims it
  between 3 and 4 s from now, with no notification.
- [x] **AC4** Given a run that inserts events, the router routes them and the resulting immediate
  deliveries are claimed within 2 s of the run's commit, without any safety poll.
- [x] **AC5** Given a delivery made due by a manual retry, a deliverer claims it within 1 s.
- [x] **AC6** Given two worker replicas and a run-now, the watch runs once (existing 001-AC9 holds) and
  both replicas record a `notify` wakeup.
- [x] **AC7** Given the LISTEN connection killed (`pg_terminate_backend`), the process reconnects, every
  loop records one wake once it listens again, and a run-now made after reconnection is claimed within 1 s.
- [x] **AC8** Given LISTEN unavailable, a due watch is still claimed by the safety poll
  (`GEDOUR_WORKER_POLL`), and the loss shows in the log (`work_listen_failed`).
- [x] **AC9** Given a due watch held under a row lock by another transaction, the watch loop makes at
  most one pass per second until the lock goes.
- [x] **AC10** Given 1000 run-now notifications in one second, each loop makes at most 2 passes for them.
- [x] **AC11** A rolled-back write to `watches` wakes no worker.
- [x] **AC12** Given a busy Talk channel, its held deliveries are retried 1 s later, whatever
  `GEDOUR_WORKER_POLL` is.
- [x] **AC13** Shutdown still drains in-flight runs and deliveries within `GEDOUR_SHUTDOWN_TIMEOUT`
  while a loop is asleep on a deadline.

## Phases

### Phase 1 — Triggers and the work listener
- Work: migration with the trigger function and the three statement-level triggers; a `pkg/wake`
  listener (dedicated connection, reconnect with backoff, fan-out to per-loop wake signals, wake-all on
  (re)connect). Not yet used by the loops.
- **Covers**: AC11
- **Data model impact**: function `gedour_work_notify()`, triggers on `watches`, `events`,
  `deliveries`. No column, no table rewrite.
- **DoD**: migration up and down on a fresh DB and on the current schema; a testpg test sees one
  notification per statement on each table and none after a rollback; a test kills the listen backend
  and sees the reconnect and the wake-all.

### Phase 2 — Deadline sleep in the three loops
- Work: shared sleep helper (deadline, wake, safety poll, jitter, `minPass`, error backoff); deadline
  queries for watches and deliveries; worker, router and deliverer wired to it and to the listener;
  Talk busy delay constant; `gedour_worker_wakeups_total`; `gedour_watches_due` per pass; new
  `GEDOUR_WORKER_POLL` default.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6, AC7, AC8, AC9, AC10, AC12, AC13
- **Data model impact**: none
- **DoD**: `go test ./pkg/worker/... ./pkg/route/... ./pkg/app/...` green with a test per AC; existing
  worker, router and delivery suites green unchanged; golangci-lint clean; `scripts/e2e.sh --workers=4`
  green.

### Phase 3 — Docs and the lab
- Work: README rows and paragraphs listed in Scope; `deploy/k8s/test` keeps `GEDOUR_WORKER_POLL=1s`
  as the safety poll (tests stay fast if a notification is lost).
- **Covers**: none
- **Data model impact**: none
- **DoD**: `grep -n "GEDOUR_WORKER_POLL" README.md` shows only the new meaning; on the lab, idle
  `gedour_worker_wakeups_total` over 10 min shows ≤ 45 passes per pod, and a run-now from the UI runs at
  once (`last_run_at` within 2 s).

## Data model impact (summary)

- Function `gedour_work_notify()` and three statement-level triggers (`watches`, `events`,
  `deliveries`). No new column. The extra cost per write is one `pg_notify` per statement.

## Open questions

- None.

## Decisions

- Triggers over `pg_notify` calls in Go — at least eight call sites move `next_run_at` or
  `next_attempt_at` today (watch create, edit, pause, run-now, bulk run-now, user re-enable, transfer,
  delivery retry, digest), and a trigger also covers future ones.
- Lease expiry stays out of the deadline — the safety poll (60 s) is far below the lease (5 min, 2 min
  for deliveries), so a crashed replica's work is late by at most one safety poll; notifying on lease
  writes would wake every replica on every claim.
- `GEDOUR_WORKER_POLL` keeps its name and becomes the safety poll, default 60 s — a deployment that set
  it keeps working, only faster or slower to recover from a lost notification.
- Jitter of 0 to 250 ms and `minPass` of 1 s are constants, not settings — nothing needs to tune them
  per deployment.
- All replicas wake on every notification — one cheap deadline query per wake; picking one replica
  would need coordination that SKIP LOCKED already makes unnecessary.
- `minPass` = min(1 s, poll), jitter < min(250 ms, poll/4) — bounded by the safety poll, so a short test
  poll stays fast.
- Listener and loop helper live in `pkg/wake`, used by `pkg/worker` and `pkg/route`.

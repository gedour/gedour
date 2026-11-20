# 019 — GitHub scheduled workflows deadman

**Status**: in progress
**Requested by**: babs
**Date**: 2026-11-19

## Problem

GitHub turns off the scheduled workflows of a public repository after 60 days without repository activity.
No notification goes out at that point, and the cron line stays in the YAML. Scheduled runs can also be
delayed or skipped when GitHub is under load, or keep failing for weeks. Nobody notices until whatever the
job maintained (a mirror, a cache, a report) has gone stale.

## Solution

A new watch kind, `github_schedules`, follows every scheduled workflow of one repository. It finds them
itself: each workflow whose file on the default branch has an `on.schedule` entry. It sends an event when
one of them:

- leaves the `active` state (disabled for inactivity, by hand, or in a fork),
- misses N expected runs in a row, with the expected times computed from its own cron lines,
- comes back.

Two opt-ins are added on top:

- a warning N days before the 60-day cutoff (public repositories),
- re-enabling a workflow that GitHub disabled for inactivity, through the watch's credential.

## Scope

- Kind `github_schedules`: Go constant, `Kinds`, `defaultHosts` (`api.github.com`), `CredentialKindFor`
  (`github_token`), `DefaultScheduleFor` (`1h`, like the other GitHub kinds), API enums, OpenAPI, web types,
  MCP, README, rule reference.
- Config:
  - `repo`, `api_url` (default `https://api.github.com`), validated like `github_repo`.
  - `alive_on`: `success` (default) or `completed`. It sets which run counts as proof of life.
  - `missed_runs`: integer 1 to 10, default 3.
  - `warn_days`: integer 0 to 30, default 0 (off).
  - `auto_enable`: boolean, default `false`.
- Requests per run:
  1. `GET /repos/{repo}/actions/workflows?per_page=100`, every page.
  2. `GET /repos/{repo}/contents/.github/workflows`: the file list of the default branch, with the blob
     `sha` of each file.
  3. `GET /repos/{repo}/contents/{path}` (raw) for each workflow file whose `sha` differs from the stored
     one. The parsed cron lines are stored with the `sha`.
  4. `GET /repos/{repo}/actions/workflows/{id}/runs?event=schedule&status=<alive_on>&per_page=1` for each
     scheduled workflow in state `active`.
  5. With `warn_days > 0`: `GET /repos/{repo}` (visibility) and `GET /repos/{repo}/commits?per_page=1`
     (last commit on the default branch).
  6. With `auto_enable`: `PUT /repos/{repo}/actions/workflows/{id}/enable` for each workflow in
     `disabled_inactivity`.
- Workflow discovery: a workflow listed by (1) whose path is a file in (2) and whose YAML has at least one
  `on.schedule[].cron`. Other workflows, including dynamic ones (CodeQL, Dependabot), are ignored. `on` as
  a string or a list never holds a schedule.
- Expected run times: union of the workflow's cron lines, parsed with `robfig/cron` (already a dependency).
  A `timezone` field becomes a `CRON_TZ=` prefix. Without one, the times are UTC.
- Staleness: the reference time is the later of the last proof-of-life run's `created_at` and the
  workflow's `updated_at`. A workflow is stale when at least `missed_runs` expected times fall after that
  reference and more than 1 h before now. Only `active` workflows can be stale.
- State, per scheduled workflow: `id`, `name`, `path`, `file_sha`, `crons` (cron and timezone), `state`,
  `url`, `last_alive_at`, `stale`, `reenabled_at`. Repository level: `alive_on`, `private`,
  `last_activity_at`, `disable_at`, `disable_soon`.
- Events (subject: the workflow path, or the repository for `schedule.disable_soon`):
  - `schedule.disabled`: `active` to any other state. Payload: `workflow`, `path`, `state` (to),
    `last_alive_at`, `url`, plus `reenable_error` when an auto-enable failed.
  - `schedule.enabled`: back to `active`.
  - `schedule.stale`: `stale` false to true. Payload adds `missed` (count) and `expected_at` (the oldest
    missed time).
  - `schedule.recovered`: `stale` true to false.
  - `schedule.reenabled`: gedour re-enabled the workflow this run. Payload adds `from` (`disabled_inactivity`).
  - `schedule.disable_soon`: `disable_soon` false to true. Payload: `repo`, `last_activity_at`,
    `disable_at`, `workflows` (paths of the active scheduled workflows).
- First run: unlike other kinds, a `github_schedules` watch reports on its first run. It sends
  `schedule.disabled`, `schedule.stale` and `schedule.disable_soon` for what is already broken. The kind
  opts in, and the worker diffs the first state against an empty one. A healthy repository emits nothing.
  Other kinds keep 001-AC8.
- Pre-warning (`warn_days > 0`, public repository only): `disable_at` = last default-branch commit date +
  60 days. `disable_soon` is true when now ≥ `disable_at - warn_days` and at least one scheduled workflow
  is `active`. A newer commit moves `disable_at` and resets the flag.
- Auto-enable (`auto_enable`): only `disabled_inactivity` gets re-enabled. `disabled_manually`
  and `disabled_fork` are human or fork decisions, and they get `schedule.disabled`. A successful
  PUT stores `state: active` and `reenabled_at`. A failed PUT (403, 404, 5xx) does not fail the run: it
  stores the disabled state, sends `schedule.disabled` with `reenable_error`, and the next run tries again.
  Each PUT is logged as `workflow_reenabled` or `workflow_reenable_failed`.
- `auto_enable` needs a credential. The API refuses `auto_enable: true` without one (400), and refuses
  turning it on through an API key (403).
- Message summaries in `pkg/notify/message.go` and `web/src/lib/events.ts` for the six event types.
- UI: kind picker entry "GitHub scheduled workflows". Form fields: repo, a select for `alive_on` ("Last
  successful run" or "Last run, any result"), `missed_runs`, `warn_days`, `auto_enable` with a hint that the
  token needs Actions write. A hint gives the cost: 2 requests plus 1 per scheduled workflow per run, plus 2
  with the warning. Pasting `github.com/<o>/<r>/actions` picks this kind.
- Fake upstream fixtures a/b for every endpoint above, including the PUT. README section with the token
  permissions: Actions read, Contents read (private repositories), Metadata read, and Actions write for
  `auto_enable`.

## Out of scope

- Workflows outside `.github/workflows` on the default branch. GitHub only schedules from there.
- Reporting a workflow that gains or loses its schedule, or a workflow file that does not parse. Those
  workflows are left out of the state and nothing is sent.
- Keeping a repository alive (dummy commits) to stop the 60-day clock.
- GitLab pipeline schedules.
- Thresholds per workflow. `missed_runs` applies to the whole repository.
- Re-enabling `disabled_manually` or `disabled_fork` workflows.
- Watching one workflow only. A rule filter on `event.payload.path` does it.

## Acceptance criteria

- [ ] **AC1** The API refuses a `github_schedules` config with a bad `repo`, `missed_runs` outside 1 to 10,
  `warn_days` outside 0 to 30, an `alive_on` other than `success`/`completed`, or `auto_enable: true` with
  no credential. Each gets a field error and a 400.
- [ ] **AC2** In fixture a, a repository holds a scheduled workflow, a push-only workflow, a workflow with
  `on: [push]`, and a dynamic CodeQL workflow. Only the scheduled one is in the state.
- [ ] **AC3** A second run with unchanged workflow files makes no contents request for a workflow file.
  Changing one file's `sha` fetches that file only.
- [ ] **AC4** Going from `active` to `disabled_inactivity` emits one `schedule.disabled` with the payload
  above. The next unchanged run emits nothing. Back to `active` emits one `schedule.enabled`.
- [ ] **AC5** Hourly cron `0 * * * *`, `missed_runs: 3`, last success at 08:05 UTC: a run at 11:59 emits
  nothing, a run at 12:01 emits `schedule.stale` with `missed: 3` and `expected_at` 09:00.
- [ ] **AC6** With `alive_on: success`, a failed run after the last success does not count. With
  `alive_on: completed`, it does and the workflow is not stale.
- [ ] **AC7** `cron: '30 5 * * 1-5'` with `timezone: "America/New_York"` expects 09:30 or 10:30 UTC
  depending on DST, and two cron lines on one workflow count as one union of times.
- [ ] **AC8** A stale workflow whose next run lands emits `schedule.recovered`.
- [ ] **AC9** The first run of a watch on a repository with one disabled and one stale workflow emits one
  `schedule.disabled` and one `schedule.stale`. On a healthy repository it emits nothing.
- [ ] **AC10** A failing call (any of 1 to 5) fails the run like any collector: no state saved, no events.
- [ ] **AC11** Telegram, email, Talk and the UI show one readable line for each event type (e.g.
  "Scheduled workflow nightly.yml disabled (inactivity)", "nightly.yml missed 3 runs since 2026-11-04 09:00 UTC", from the
  oldest missed time).
- [ ] **AC12** The form shows the fields and the hints above. Pasting `https://github.com/o/r/actions` picks
  `github_schedules` with repo `o/r`. The a11y, 375 px and CSP e2e checks pass on the form.
- [ ] **AC13** On a public repository whose last commit is 54 days old, with `warn_days: 7`, one
  `schedule.disable_soon` goes out with `disable_at` = commit + 60 d. The next run emits nothing. A new commit
  clears the flag. A private repository never gets it. With `warn_days: 0`, neither `/repos` nor
  `/commits` is called.
- [ ] **AC14** With `auto_enable`, a `disabled_inactivity` workflow gets one PUT and one
  `schedule.reenabled`, and no `schedule.disabled`. A `disabled_manually` one gets no PUT and a
  `schedule.disabled`.
- [ ] **AC15** A PUT answering 403 emits `schedule.disabled` with `reenable_error`, the run succeeds, and
  the next run tries the PUT again.
- [ ] **AC16** Turning `auto_enable` on through an API key (REST or MCP) is refused with a 403. From a
  session, the owner can turn it on.

## Phases

### Phase 1 — Detect disabled and stale schedules
- Work: migration, kind constant and enums, config and validation, collector (requests 1 to 4), workflow
  YAML parsing, cron expectation, diff, first-run opt-in in the worker, summaries, kind picker, form, URL
  recognition, fixtures, `make openapi`, README.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6, AC7, AC8, AC9, AC10, AC11, AC12
- **Data model impact**: `watches.kind` CHECK gains `github_schedules` (new migration file).
- **DoD**: the migration applies on a fresh DB and on a `develop` DB. `make coverage`, `make lint` and
  `make e2e` are green, and an e2e watch shows `schedule.disabled` then `schedule.enabled` from fixtures a/b.

### Phase 2 — Pre-warning before the 60-day cutoff
- Work: `warn_days` collection (request 5), `disable_soon` state and event, summary, form field, fixtures.
- **Covers**: AC13
- **Data model impact**: none (config and state are JSON).
- **DoD**: `make coverage` and `make lint` are green, and a collector test drives AC13 with a fixed clock.

### Phase 3 — Auto re-enable
- Work: PUT through the fetcher (a write with the watch credential), `schedule.reenabled`,
  `reenable_error`, API-key refusal, log lines, form box and hint, fake upstream PUT route.
- **Covers**: AC14, AC15, AC16
- **Data model impact**: none.
- **DoD**: `make coverage`, `make lint` and `make e2e` are green. The fake upstream `/_received` shows one
  PUT for the `disabled_inactivity` fixture and none for `disabled_manually`.

## Data model impact (summary)

One migration: `github_schedules` in the `watches.kind` CHECK. Config and state are JSON. A state holds one
entry per scheduled workflow.

## Open questions

- [ ] None.

## Decisions

- One watch per repository with auto-discovery, asked by babs. A `missed_runs` count gives the threshold, in
  expected runs rather than a duration, so one watch fits an hourly and a weekly workflow.
- The cost of discovery stays low. The directory listing returns each file's `sha`, and a file is fetched
  again only when its `sha` changes. Steady state is 2 + S requests (S = active scheduled workflows).
- 1 h grace before an expected time counts as missed. GitHub delays scheduled runs under load. With the
  default `missed_runs: 3`, one skipped hourly run does not fire an alert.
- The proof-of-life rule is a form choice (`alive_on`), asked by babs. The default is `success`, so a job
  that fires but always fails also raises `schedule.stale`.
- The workflow's `updated_at` counts as a reference time. A workflow just re-enabled or edited is not stale
  for the misses before that.
- The first run reports problems, a break from 001-AC8: a deadman created on an already disabled workflow
  must say so. The opt-in is per kind.
- A deleted workflow file leaves the directory listing, so the workflow leaves the state with no event: deleting
  the file is a deliberate act.
- Every non-`active` state of a listed workflow fires `schedule.disabled`, `disabled_manually` included. A rule filter on
  `event.payload.state` can drop the ones you do not want.
- The 60-day prediction uses the last commit on the default branch, not `pushed_at`. GitHub does not
  document what counts as activity. Third-party sources point at commits on the default branch, and that
  date is the earlier one, so the warning comes early rather than late. Unverified: re-enabling through the
  API may not reset the 60-day clock (the gh-action-keepalive README says toggling did not), so the warning
  can come back after an auto-enable.
- Pre-warning only on public repositories: GitHub's docs state the 60-day rule for public repositories.
- `auto_enable` is refused through API keys. It is the first call where gedour writes to an upstream with the
  owner's token, so turning it on takes a signed-in session.
- A failed PUT does not fail the run. Detection is the main job, and a missing Actions write permission
  must not hide the `schedule.disabled` event.
- `schedule.recovered` needs a proof-of-life run later than the stored one, under the same `alive_on`: raising
  `missed_runs`, switching `alive_on` or editing the workflow clears `stale` with no event.
- When `X-RateLimit-Remaining` is below the calls a run still needs, the run stops before fetching files, with an
  error naming the count (and a credential, when there is none). It waits for `X-RateLimit-Reset` like any rate
  limit, without counting as a failure. When the run needs more than `X-RateLimit-Limit`, it fails instead.
- `missed` counts every missed time up to 1000 on the stale transition, shown "1000+" at the cap. While the
  workflow stays stale the count is kept as long as its reference does not move, so the state does not change
  run to run. A newer proof-of-life run
  under the same `alive_on`, followed by new misses before the next run, recounts and sends `schedule.stale`
  again: the job came back and died again.
- Messages read "missed N runs since <expected_at>": the oldest missed time, since the count starts at the
  workflow's last edit when that is later than its last run.
- PUT results go to logs, not to `audit`. The audit table records user mutations made through the API.
  The event and its `reenable_error` already record the outcome.

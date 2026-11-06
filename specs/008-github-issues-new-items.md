# 008 — GitHub repository watches: new issues and pull requests, one-issue watches

**Status**: in progress
**Requested by**: babs
**Date**: 2026-11-06

## Problem

A GitHub repository watch reports releases only. Nobody hears about a new issue or a new pull request on a
repository they follow, short of checking it by hand. `github_pr` follows one PR, but there is no way to
follow one issue: its collector calls `/pulls/{number}`, which answers 404 for an issue.

## Solution

The `github_release` kind becomes `github_repo`, everywhere including stored data. A `github_repo` watch
gets two opt-in boxes, "new issues" and "new pull requests". Each issue or PR opened after the box is
checked produces one event. A new `github_issue` watch kind follows one issue: open/closed (with the
reason), labels, and new comments, the same way `github_pr` follows one PR.

## Scope

- Rename the kind `github_release` to `github_repo`: Go constant and config type, API enums (watches,
  rules, scripts, filter `watch.kind`), OpenAPI, web types and labels, MCP, fixtures, README, e2e.
- Migration: `watches.kind` CHECK and rows move to `github_repo`; the quoted literal `"github_release"`
  (double, single and backtick quotes) in saved rule filters becomes `"github_repo"`. Down migration reverses all of it.
- `github_repo` config: `new_issues` and `new_prs`, booleans, default `false`.
- With either box checked, one extra call per run:
  `GET /repos/{repo}/issues?state=all&sort=created&direction=desc&per_page=50`. It lists issues and PRs; a PR
  carries a `pull_request` key. Neither box checked: no extra call.
- Events `issue.opened` and `pr.opened`. Payload: `number`, `title`, `author`, `labels` (names), `url`,
  `created_at`. Subject: the number.
- The repository state stores the numbers seen (the 50 newest). A state with no numbers stored (watches
  older than this spec, or a box just checked) takes the current list as baseline and emits nothing.
- An item with a number at or below the oldest number stored is not new (it slid back into the window).
- New kind `github_issue`: config `repo`, `number`, `api_url`; credential `github_token`; default schedule
  `1h`, like `github_pr`.
- `github_issue` state: `state` (`open`, `closed`), `state_reason` (as GitHub reports it, e.g.
  `completed`, `not_planned`, or empty), `title`, `labels` (sorted), `url`, and the 20 newest comments, read
  with `lastPage` and normalized like PR conversation comments.
- `github_issue` events: `issue.state_changed` (from/to, plus `state_reason`), `issue.labels_changed`
  (from/to), `issue.commented` (author, excerpt), built with `diffFacets` and `newComments`.
- A `github_issue` watch on a number that is a PR fails with a clear error naming `github_pr`.
- UI: the kind picker shows "GitHub repository" and "GitHub issue"; the `github_repo` form shows both boxes
  with a hint that each run then costs two requests (60 an hour without a token). A pasted
  `github.com/<o>/<r>/issues/<n>` URL picks `github_issue`.
- Message summaries (`pkg/notify/message.go` and `web/src/lib/events.ts`) for the five new event types.
- Fake upstream fixtures a/b for the issues list and one issue; README and rule reference.

## Out of scope

- Accepting `github_release` as an alias on the API after the rename.
- Rewriting user scripts that compare the kind to `"github_release"`: the release notes say so.
- Closing or merging of items reported by the new-items boxes: follow the item with `github_pr` or
  `github_issue`.
- Filters on label or author in the watch config: a rule filter on `event.payload.labels` or
  `event.payload.author` does it.
- Issue and PR bodies in payloads.
- GitLab issues and merge requests.
- A "watch this item" button on `issue.opened` / `pr.opened` events.
- Issue edits other than state, labels and comments (title, assignees, milestone).

## Acceptance criteria

- [ ] **AC1** After the migration, every former `github_release` watch has kind `github_repo`, keeps its
  config, states and events, and its next run emits nothing new; the down migration restores
  `github_release`.
- [ ] **AC2** A saved rule filter `watch.kind == "github_release"` (or single- or backtick-quoted) reads `github_repo`
  after the migration and still routes that watch's events.
- [ ] **AC3** The API refuses `kind: "github_release"` with a 400 (the project maps 422 to 400) listing the valid kinds; `grep -rn
  github_release` outside migrations, specs 001-007 and the changelog returns nothing.
- [ ] **AC4** A `github_repo` watch with both boxes off makes exactly the requests a `github_release` watch
  made, and its events do not change.
- [ ] **AC5** With `new_issues` on, an issue opened between two runs produces one `issue.opened` with the
  payload above; a PR opened in the same interval produces nothing unless `new_prs` is on, then one
  `pr.opened`.
- [ ] **AC6** The first run after a box is checked, and the first run of a watch stored before this spec,
  emit no `issue.opened` or `pr.opened`.
- [ ] **AC7** An item that reappears in the window after a deletion, with a number below the oldest stored,
  emits nothing.
- [ ] **AC8** A failing issues call fails the run like a failing releases call: no partial state saved, no
  events.
- [ ] **AC9** A `github_issue` watch reports a close as `issue.state_changed` with `state_reason`, a reopen
  the same way, a label added or removed as `issue.labels_changed`, and each new comment as
  `issue.commented`; its first run emits nothing.
- [ ] **AC10** A `github_issue` watch on a PR number fails with an error that names `github_pr`.
- [ ] **AC11** The API refuses a `github_issue` config with a bad `repo` or a non-positive `number`, with
  field errors like `github_pr`'s.
- [ ] **AC12** Telegram, email, Talk and the UI show one readable line for each new event type (e.g.
  "Issue #12 opened by alice", "Issue #12 closed (not planned)").
- [ ] **AC13** Pasting `https://github.com/o/r/issues/12` in the watch form picks `github_issue` with repo
  `o/r` and number 12.
- [ ] **AC14** A rule filter `"bug" in event.payload.labels` routes an `issue.opened` fixture event.
- [ ] **AC15** The form hint on the two boxes states the doubled request count; a11y, 375 px and CSP e2e
  checks pass on the form.

## Phases

### Phase 1 — Rename `github_release` to `github_repo`
- Work: migration (up and down), constants, enums, OpenAPI, web, MCP, fixtures, README, e2e.
- **Covers**: AC1, AC2, AC3
- **Data model impact**: `watches.kind` CHECK and rows; `rules` filter text (literal rewrite).
- **DoD**: migration up, down, up on a DB seeded with a `github_release` watch and rule; `make coverage`,
  `make lint`, `make e2e` green; the AC3 grep returns nothing.

### Phase 2 — New issues and PRs on repository watches
- Work: config fields and validation, issues call, state numbers, diff, summaries, form boxes and hint,
  fixtures, README.
- **Covers**: AC4, AC5, AC6, AC7, AC8, AC12, AC14, AC15
- **Data model impact**: none (config and state are JSON).
- **DoD**: `make coverage` and `make lint` green; `make e2e` green with a rule routing a fixture
  `issue.opened` to the inbox.

### Phase 3 — `github_issue` watch kind
- Work: migration, kind constant and enums, config, collector, diff, summaries, kind picker, URL
  recognition, fixtures, `make openapi`, README.
- **Covers**: AC9, AC10, AC11, AC12, AC13
- **Data model impact**: `watches.kind` CHECK gains `github_issue` (new migration file).
- **DoD**: migration applies on a fresh DB and on a phase 2 DB; `make coverage`, `make lint` and `make e2e`
  green; an e2e `github_issue` watch shows a close then a comment from fixtures a/b.

## Data model impact (summary)

Two migrations: the kind rename (rows, CHECK, rule filter literals) and `github_issue` in the CHECK.
Repository states grow by up to 50 numbers.

## Open questions

- [ ] None.

## Decisions

- Rename to `github_repo` with no API alias, asked by babs ("renomme le type propre"). Breaks API and MCP
  clients and scripts that send or compare `github_release`; pre-1.0, stated in the release notes.
- Rule filters are rewritten by literal match only: a filter that builds the string another way keeps the
  old value and stops matching.
- One issues call for both boxes: the `/issues` list returns PRs too, so `new_prs` alone costs the same as
  both.
- Baseline on first run instead of announcing the existing 50 items, as `github_pr` does for comments.
- One list of seen numbers per kind (`issues`, `prs`), stored only while its box is on: checking a second
  box baselines that kind alone. A box is not part of the target: toggling it keeps the release states and
  drops the watch's cached ETags.
- A box is part of the config for authz: on a watch with a credential, only its owner, from the UI, toggles
  it (the box spends the owner's token on another call).
- With a box on, both requests are unconditional: a 304 on one list would end the run before the other.
- Default schedule `1h` for `github_issue`, like `github_pr`: the unauthenticated GitHub budget is 60
  requests an hour.

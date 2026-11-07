# 011 — GitHub discussions and reactions

**Status**: shipped
**Requested by**: babs
**Date**: 2026-11-07

## Problem

gedour does not follow GitHub Discussions. A new discussion on a followed repository goes unseen, and so
does the close, the chosen answer or a new comment on a discussion someone took part in. Reactions are
not followed anywhere either: the author of an issue, a PR or a discussion does not learn that people
upvoted it, or reacted to one of their comments.

## Solution

A `github_repo` watch gets a third box, "new discussions": each discussion opened after the box is checked
produces one event. A new `github_discussion` watch kind follows one discussion: open/closed (with the
reason), chosen answer, labels and new comments, replies included. `github_issue`, `github_pr` and
`github_discussion` watches get a "reactions" box: a rise in the reaction counts on the item itself or on
one of its recent comments produces one event.

## Scope

- Discussions are read through the REST endpoints `GET /repos/{repo}/discussions`,
  `/discussions/{n}` and `/discussions/{n}/comments`. They answer on the `core` rate limit with ETags, but
  GitHub does not document them (github/docs#44098).
- `github_repo` config: `new_discussions`, boolean, default `false`, `omitempty` like `new_issues`.
- With the box checked, each run reads the discussions list. The list comes back oldest first and
  ignores `sort`/`direction`, so the run first asks `per_page=1`: its `Link` rel="last" page number is the
  total. It then reads the last `per_page=100` page, and the page before it when the last one holds fewer
  than 50 items. Only those pages make the window. That adds 2 to 3 requests per run (1 when the
  repository has at most one discussion). A `Link` whose last page number cannot be read fails the run.
- Event `discussion.opened`. Payload: `number`, `title`, `author`, `category`, `labels` (names), `url`,
  `created_at`. Subject: the number.
- The repository state stores the numbers seen, as `discussions`, with the same baseline rule as
  `issues` and `prs` (008): no stored list means baseline. Only a number above the highest stored is new.
- A 410 on the list ("Discussions are disabled for this repo") fails the run with an error naming the
  box.
- New kind `github_discussion`: config `repo`, `number`, `api_url`, `reactions`; credential
  `github_token`; default schedule `1h`.
- `github_discussion` state: `number`, `state` (`open`, `closed`), `state_reason` (as GitHub reports it, e.g.
  `resolved`, `outdated`, `duplicate`, or empty), `title`, `category`, `labels` (sorted), `answer_url`
  (empty when none), `answer_chosen_by` (for `chosen_by`), `url`, and the 20 newest comments, read with
  `lastPage` and normalized like issue comments. The comments list is flat and holds replies too: a reply
  carries `reply_to`, the URL of its parent comment.
- `github_discussion` events: `discussion.state_changed` (from/to plus `state_reason`, also on a reason
  change while closed, like `issue.state_changed`), `discussion.answer_changed` (from/to answer URLs,
  plus `chosen_by`), `discussion.labels_changed` (from/to), `discussion.commented` (author, excerpt, and
  `reply_to` on a reply).
- A `github_discussion` watch on a number that is not a discussion (404) fails with an error that names
  `github_issue` and `github_pr`: issues, PRs and discussions share one numbering.
- Config `reactions`, boolean, default `false`, `omitempty`, on `github_issue`, `github_pr` and
  `github_discussion`.
- With `reactions` on, the state stores the counts of the 8 reaction kinds (`+1`, `-1`, `laugh`,
  `hooray`, `confused`, `heart`, `rocket`, `eyes`) for the initial post (the issue, PR or discussion
  body) and for each of the 20 newest comments.
  `total_count` and `url` are dropped.
- Extra requests: none for `github_issue` and `github_discussion`, whose responses already carry the
  counts. `github_pr` adds `GET /repos/{repo}/issues/{n}` for the PR body counts. Inline review comments
  carry counts; review bodies have none and are not tracked.
- Event `<kind>.reacted` (`issue.reacted`, `pr.reacted`, `discussion.reacted`), one per target whose
  counts rose since the last run. Payload: `number`, `title`, `url` (the item or the comment), `target`
  (`item` or `comment`), `added` (reaction kind to the rise, rises
  only), `counts` (current counts). A drop updates the state and emits nothing.
- UI: kind picker gets "GitHub discussion". The `github_repo` form shows the third box with a hint on its
  extra requests. The three single-item forms show the reactions box. A pasted
  `github.com/<o>/<r>/discussions/<n>` URL picks `github_discussion`.
- Message summaries (`pkg/notify/message.go`, `web/src/lib/events.ts`) for the new event types.
- Fake upstream fixtures a/b for the discussions list, one discussion, its comments, and reactions on an
  issue and a PR; README and rule reference.

## Out of scope

- The GraphQL API, and a fallback to it if GitHub removes the REST endpoints.
- GitHub Enterprise Server: the endpoints were checked on github.com only.
- Who reacted: the REST `/reactions` endpoints answer 404 for discussions, and the counts are enough.
- Reactions on comments outside the 20 stored, on review bodies (and on a PR entry folding several inline
  comments of one review), and on items reported by the new-items boxes of `github_repo`.
- Reactions on GitLab items.
- Discussion upvotes (`upvote_count` is null in the REST answer), category changes, locks, title or body
  edits.
- Discussion bodies in payloads.
- A "watch this discussion" button on `discussion.opened` events.

## Acceptance criteria

- [x] **AC1** A `github_repo` watch with `new_discussions` off makes exactly the requests it made before
  this spec, and its events do not change.
- [x] **AC2** With `new_discussions` on, a discussion opened between two runs produces one
  `discussion.opened` with the payload above.
- [x] **AC3** The first run after the box is checked emits no `discussion.opened`; checking it makes the
  watch due now, like `new_issues`.
- [x] **AC4** A last page of 14 discussions plus a full page before it: a discussion opened at the end of
  the page before it is reported, and the run made at most 3 extra requests.
- [x] **AC5** A discussion that reappears in the window with a number below the oldest stored emits
  nothing.
- [x] **AC6** A 410 or a failing call on the discussions list fails the run with no partial state and no
  events; the 410 error names the "new discussions" box.
- [x] **AC7** A `github_discussion` watch reports a close as `discussion.state_changed` with
  `state_reason`, a reopen the same way, a chosen or removed answer as `discussion.answer_changed`, a
  label change as `discussion.labels_changed`, and each new comment or reply as `discussion.commented`
  (`reply_to` set on the reply); its first run emits nothing.
- [x] **AC8** A `github_discussion` watch on an issue number fails with an error that names `github_issue`
  and `github_pr`.
- [x] **AC9** The API refuses a `github_discussion` config with a bad `repo` or a non-positive `number`,
  with field errors like `github_issue`'s.
- [x] **AC10** Pasting `https://github.com/o/r/discussions/12` in the watch form picks
  `github_discussion` with repo `o/r` and number 12.
- [x] **AC11** With `reactions` off, `github_issue`, `github_pr` and `github_discussion` watches make the
  same requests as before and their stored states do not change.
- [x] **AC12** With `reactions` on, two hearts added to the item and one `+1` added to a stored comment
  between two runs produce two `<kind>.reacted` events: one with `target: "item"` and
  `added: {"heart": 2}`, one with `target: "comment"`, the comment URL and `added: {"+1": 1}`. This
  holds for each of the three kinds.
- [x] **AC13** The first run after `reactions` is checked, and the first run of a state stored before
  this spec, emit no `*.reacted`; a reaction removed between two runs emits nothing.
- [x] **AC14** A `github_pr` watch with `reactions` on makes one extra request per run
  (`/issues/{n}`); `github_issue` and `github_discussion` make none.
- [ ] ~~**AC15** A rule filter `event.payload.author == "alice"` routes a `pr.reacted` fixture on alice's
  comment and drops one on bob's.~~
- [x] **AC16** Telegram, email, Talk and the UI show one readable line for each new event type (e.g.
  "Discussion #12 opened by alice", "Answer chosen on discussion #12", "+2 ❤️ on issue #12").
- [x] **AC17** The form hints state the extra request counts; a11y, 375 px and CSP e2e checks pass on the
  `github_repo` and `github_discussion` forms.

## Phases

### Phase 1 — New discussions on repository watches
- Work: config field and validation, paged list read, state numbers, diff, summary, form box and hint,
  fixtures, README.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6, AC16 (`discussion.opened`), AC17 (`github_repo` form)
- **Data model impact**: none (config and state are JSON).
- **DoD**: `make coverage` and `make lint` green; `make e2e` green with a rule routing a fixture
  `discussion.opened` to the inbox.

### Phase 2 — `github_discussion` watch kind
- Work: migration, kind constant and enums (watches, rules, scripts, filter `watch.kind`, MCP), config,
  collector, diff, summaries, kind picker, URL recognition, fixtures, `make openapi`, README.
- **Covers**: AC7, AC8, AC9, AC10, AC16 (discussion events), AC17 (`github_discussion` form)
- **Data model impact**: `watches.kind` CHECK gains `github_discussion` (new migration file).
- **DoD**: migration applies on a fresh DB and on a phase 1 DB; `make coverage`, `make lint` and
  `make e2e` green; an e2e `github_discussion` watch shows a close then a reply from fixtures a/b.

### Phase 3 — Reactions on issues, PRs and discussions
- Work: `reactions` config field on the three kinds, counts in item and comment state, diff, PR extra
  request, summaries, form boxes, fixtures, README.
- **Covers**: AC11, AC12, AC13, AC14, AC16 (`*.reacted`)
- **Data model impact**: none (config and state are JSON).
- **DoD**: `make coverage`, `make lint` and `make e2e` green; an e2e `github_issue` watch with the box on
  shows an `issue.reacted` from fixtures a/b.

## Data model impact (summary)

One migration: `github_discussion` in the `watches.kind` CHECK. Repository states grow by up to 150
numbers with the discussions box on; item states grow by 8 counts per tracked target with reactions on.

## Open questions

- [ ] None.

## Decisions

- Undocumented REST over GraphQL, chosen by babs: it reuses the GET fetcher, ETags and `lastPage`, and
  works without a token. GitHub may change or drop it without notice; the run then fails with the HTTP
  error, and GraphQL becomes a new spec.
- The page before the last is read when the last holds fewer than 50 items, so the window never drops
  below 50 discussions, the size 008 found enough for issues. A `per_page=1` probe finds the last page
  instead of downloading the oldest 100 discussions.
- A discussion is new only above the highest number stored, not the lowest as for issues: GitHub lists
  a converted issue among the newest with its old lower number, and that floor would turn one deletion
  into up to 100 false `discussion.opened`. A converted issue keeps its number: it is skipped at or
  below the highest stored, and reported otherwise (a recent issue, or any conversion on an empty list).
- A 410 fails the run, like a failing issues call (008-AC8), instead of hiding the box's failure behind
  release events that still flow.
- Reaction events on rises only: a removal is rarely news, and reporting it would double the noise.
- One event per target and run, with every kind that rose, not one per reaction: a run every hour on a
  popular item would otherwise flood.
- Reactions are tracked on the initial post and the 20 newest comments, asked by babs: older comments
  need more requests per run, unbounded on a long thread.
- Toggling `reactions` or `new_discussions` is no retarget, like the 008 boxes. Checking `reactions` does
  not make the watch due now, since reactions before the first run only shift the baseline;
  checking `new_discussions` does, like `new_issues`.
- Reactions are not offered on `github_repo` items: that would mean one request per item per run.
- Spec number 011: 010 (Web Push) is in flight in this checkout.

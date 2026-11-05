# 004 — Release notes and CVE ids in events

**Status**: in progress
**Requested by**: babs
**Date**: 2026-11-05

## Problem

A release event only carries its tag, name, prerelease flag and URL. The release notes are read from the
forge and dropped, so a rule cannot select the release that fixes a CVE, which is usually stated only in
the notes. A filter on `event.payload.name` misses nearly all of them.

## Solution

GitHub and GitLab release events carry their notes in `payload.body`, capped at 64 KiB, and the CVE ids
found in the whole notes in `payload.cves`. A rule can then read
`len(event.payload.cves) > 0`, `"CVE-2026-1234" in event.payload.cves`, or a pattern of its own on
`event.payload.body`. Scripts see the same two fields.

## Scope

- `github_release` (`body`) and `gitlab_release` (`description`) events, new and promoted.
- `payload.body`: the notes, cut to 64 KiB on a UTF-8 boundary.
- `payload.cves`: ids matching `CVE-[0-9]{4}-[0-9]+` as a whole word, case-insensitive, uppercased,
  deduplicated, in order of first appearance, at most 200, each at most 32 bytes (a longer match is ignored:
  no real id is that long, and it would escape every size bound). Extracted before the cut, so an id past
  64 KiB counts.
- A release without notes: `body` is an empty string and `cves` an empty list, so a filter never has to
  test for a missing field.
- Filters read `event.payload.body` up to 64 KiB; every other string stays at 4 KiB. The cost model prices
  this one field at 64 KiB and the whole-payload bound grows from 64 KiB to 128 KiB, so `body` cannot crowd
  out the other fields.
- The filter cost budget goes from 1,000,000 to 4,000,000 units, for every filter, so a pattern of up to 61
  instructions can run on `body` (64 on other fields). Reference, README and timing tests follow: about 45 ms per filter and
  4.5 s per 100 rules in the worst case measured.
- Inbox detail panel: `cves` as a row, `body` collapsed or left out of the rows so 64 KiB of text does not
  fill the panel.
- Rule editor reference and README: both fields, and what a filter sees of `body`.

## Out of scope

- PR and MR bodies: their events report state changes, not content.
- OCI tags: they have no notes.
- Other identifiers (GHSA, advisories): a filter or script can look for them in `body`.
- Looking up a CVE (severity, CVSS).
- Raising the filter's limit on any field other than `body`.
- Events stored before this ships: they keep their old payload.

## Acceptance criteria

- [ ] **AC1** A new GitHub or GitLab release whose notes cite CVEs produces an event with `payload.body`
  and `payload.cves` set, the ids uppercased, deduplicated and in order.
- [ ] **AC2** Notes over 64 KiB are cut to at most 64 KiB of valid UTF-8, and a CVE id past the cut is still
  in `payload.cves`.
- [ ] **AC3** More than 200 distinct ids keep the first 200; `XCVE-2026-1` and `CVE-2026-1x` are not ids.
- [ ] **AC4** A release without notes has `body: ""` and `cves: []`; a filter on either evaluates without
  error. Any other event reads them the same way in filters, without them being stored.
- [ ] **AC5** The filter `"CVE-2026-1234" in event.payload.cves` and the filter
  `event.payload.body matches "CVE-[0-9]{4}-[0-9]+"` are accepted by the rule API and route a matching event.
- [ ] **AC6** Notes are not stored in `watch_states`: the persisted state of a watch does not grow with
  them, and an edit of a release's notes alone creates no event.
- [ ] **AC7** The inbox panel shows `cves` and stays readable with 64 KiB of notes, at 375 px too; the a11y and
  CSP checks pass.
- [ ] **AC8** Telegram, email and Talk messages do not change; webhook payloads and script events carry both
  fields.
- [ ] **AC9** A filter reads `event.payload.body` to its 64 KiB cut (a match at 60 KiB routes), and a filter on
  any other field is priced at 4 KiB as before.
- [ ] **AC10** The budget is 4,000,000 units: a 60-instruction pattern on `body` is accepted, a filter above the
  budget is refused on save with its computed cost. On hostile payloads a filter takes about 45 ms and 100 rules
  about 4.5 s (measured); the timing tests gate at 2.5 times that for slower runners.
- [ ] **AC11** While a worst-case rule set evaluates an event, other due watches and deliveries still run on time
  (routing does not block them); if it does, the fix is part of this spec.

## Phases

### Phase 1 — Collect and expose
- Work: read the notes in both collectors, extraction and cut in one shared function, payload fields,
  fixtures and expected events updated, inbox panel, rule reference, README.
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6, AC7, AC8, AC9, AC10, AC11
- **Data model impact**: none (payload and state are JSON; notes stay out of the state).
- **DoD**: `make coverage` and `make e2e` green; an e2e rule on `event.payload.cves` routes a fixture release
  to the inbox.

## Data model impact (summary)

None. Each new release event's payload grows by up to about 64 KiB in `events`.

## Open questions

- [ ] None.

## Decisions

- 64 KiB for `body` only, chosen by babs. Raising the limit for every field would price short fields such as
  `tag` at 64 KiB too, since the model prices a field at its maximum: moderate patterns on `tag` would be refused
  with no speed gained.
- Budget 4,000,000 units, chosen by babs: at 1,000,000 only about 15 instructions fit on `body`, where pattern
  searches will happen. Measured on an i7-14700F, 64 KiB of hostile text: up to about 11 ns per unit (chained
  Unicode classes), 45 ms at the budget, `CVE-[0-9]{4}-[0-9]+` 1 µs on text without "CVE". Not measured on the lab nodes. The worst case needs one
  user's 100 costly rules and hostile notes, and events are rare.
- A saved filter whose price rose above the budget is refused at compile and the router logs it at Error on
  start and counts it as no match. No compatibility path: pre-release, and the one rule on the lab has an empty
  filter. Consensus note: Codex asked for a compatibility path, Opus for the log.
- `body` is left out of the watch activity, deliveries and rule preview lists; the inbox list keeps it for the
  panel, so its hostile worst case is 12.8 MiB at limit=200 (accepted).
- The id pattern is the one babs gave, not the stricter official form (4 or more digits after the year).
- Fetch size unchanged: the 10 MiB limit already reads these notes (50 releases at GitHub's 125 KB maximum
  fit), they are just dropped today.

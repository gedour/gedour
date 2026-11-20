# 020 — Inbox filter by watch

**Status**: shipped
**Requested by**: babs
**Date**: 2026-11-20

## Problem

One noisy watch can bury the rest of the inbox. To acknowledge its events, the user has to pick its rows
one by one among everyone else's, and today the inbox has no way to show a single watch's items.

## Solution

Each inbox row shows its watch name, underlined, as a link. Clicking it shows only that watch's items, in
the current tab (Unread, All, Archived) and with the High priority filter kept. The filter shows as a
removable chip next to the other filters. The usual flow: click the noisy watch, select all, mark read,
remove the chip. The filter lives in the URL, so back, reload and shared links keep it.

For the one-click case, each row gets a third quick action, "Mark all from <watch> read", also in the
detail panel; on a list narrower than 28rem the row keeps two actions and the panel or dialog carries it. It marks every unread item of that watch read on the server, loaded or not.

## Scope

- `GET /api/v1/inbox` takes an optional `watch` query parameter (a watch ID); the MCP `list_inbox` tool
  gets it too, since its inputs come from the OpenAPI document.
- The row's watch name is a link to `/inbox?watch=<id>` (keeping `state` and `priority`), underlined on
  hover and focus.
- A chip "<watch name> ×" next to the High priority toggle while the filter is on; × removes it.
- An empty state for a filter that matches nothing, with a way to remove the filter.
- `POST /api/v1/inbox/watches/{id}/read` marks the caller's unread items of one watch read; MCP tool
  `mark_watch_inbox_read`.
- A third quick action on each row (lists at least 28rem wide) and a button in the detail panel call it, then show a toast with the
  count.

## Out of scope

- Unread and read counts per watch: the tab badges stay global.
- Several watches at once, or filters by watch kind, rule or event type.
- Server-side archive or unread for a whole watch: only "read" gets a per-watch action.
- An undo for "Mark all from <watch> read", and a confirmation dialog before it.
- A keyboard shortcut for the filter or the per-watch action.

## Acceptance criteria

- [x] **AC1** Given items from watches A and B, when `GET /api/v1/inbox?watch=<A>` is called, then only
  A's items come back, newest first, with `state`, `priority` and the cursor applied as without the filter.
- [x] **AC2** `watch` set to a watch the caller cannot read, or to an unknown ID, returns an empty list,
  never another user's items.
- [x] **AC3** `watch` that is not a UUID returns a 4xx validation error naming `query.watch`.
- [x] **AC4** The MCP `list_inbox` tool accepts `watch` and returns the same items as the HTTP call.
- [x] **AC5** Clicking a row's watch name shows only that watch's items, keeps the tab and the High
  priority toggle, and does not open the item.
- [x] **AC6** While the filter is on, a chip shows the watch name; clicking its remove button lists every
  watch again, in the same tab.
- [x] **AC7** The filter is in the URL (`?watch=<id>`): reload and browser back restore it, and the chip
  shows the watch name even when no item is loaded (the Unread tab emptied by marking all read).
- [x] **AC8** Filtered, select all then Mark read marks only the loaded items of that watch.
- [x] **AC9** A filter that matches nothing shows "Nothing from <watch name> here" with a button that
  removes the filter.
- [x] **AC10** The watch name is a real link, reachable by Tab, with an accessible name naming the watch
  (e.g. "Show only <watch name>"); the row stays valid HTML (no link nested in the row's button).
- [x] **AC11** Given 120 unread items from watch A (more than one page) and unread items from B, when
  `POST /api/v1/inbox/watches/<A>/read` is called, then all 120 of A's are read, B's are untouched, and the
  response gives `updated: 120` and the new global `unread` count; other open tabs get the new count.
- [x] **AC12** Archived items and items already read are left as they are (`read_at` of a first read is
  kept); a watch the caller cannot read, or an unknown one, gives `updated: 0` and changes nothing.
- [x] **AC13** The MCP `mark_watch_inbox_read` tool (write scope) does the same as the HTTP call.
- [x] **AC14** On a list at least 28rem wide, each row's quick actions include "Mark all from <watch
  name> read" (accessible name and tooltip); narrower, the panel or dialog carries it (AC15); clicking it marks the watch's items read, refreshes the list and the tab counts, and shows
  "N items from <watch name> marked read" (or "Nothing unread from <watch name>" when N is 0).
- [x] **AC15** The detail panel and the mobile dialog have the same action, so it works without hover.
- [x] **AC16** A failed call shows the inbox's usual error toast and changes nothing on screen.

## Phases

### Phase 1 — API filter
- Work: `watch` query parameter on `list-inbox` (`format:"uuid"`), one `AND e.watch_id = @watch` clause;
  OpenAPI regenerated (`make openapi`); README's `list_inbox` row mentions it.
- **Covers**: AC1, AC2, AC3, AC4
- **Data model impact**: none
- **DoD**: `go test ./pkg/route ./pkg/mcpserver ./pkg/server` green, including new cases for AC1-AC4;
  `cd web && pnpm check:api` passes.

### Phase 2 — Inbox UI
- Work: `listInbox` passes `watch`; the row's watch name becomes a link laid over the row's open button
  (stretched-button pattern, so the DOM stays valid); filter chip named from the loaded items, else from
  `getWatch`, always asked so the chip keeps its name once the list empties; empty state; the query key
  includes the watch.
- **Covers**: AC5, AC6, AC7, AC8, AC9, AC10
- **Data model impact**: none
- **DoD**: `pnpm exec vitest run src/pages/InboxPage.test.tsx` covers AC5-AC10; one Playwright case in
  `web/e2e/` filters, selects all, marks read and removes the chip; screenshots at 1280 and 375 px.

### Phase 3 — Mark all from a watch read
- Work: `read-watch-inbox` operation, one `UPDATE inbox_items ... FROM events, watches` limited to the
  caller, the watch, readable watches, `read_at IS NULL AND archived_at IS NULL`, in the same transaction
  and NOTIFY as `bulk`; MCP tool entry in `pkg/mcpserver/tools.go`; README tool table; row quick action
  (third button) and detail-panel button; toast.
- **Covers**: AC11, AC12, AC13, AC14, AC15, AC16
- **Data model impact**: none
- **DoD**: `go test ./pkg/route ./pkg/mcpserver ./pkg/server` green with cases for AC11-AC13 (more than
  one page of items, another user's watch, archived and already-read items); vitest covers AC14-AC16;
  the Playwright case from phase 2 also clicks the row action; screenshots at 1280 and 375 px.

## Data model impact (summary)

None. The filter and the per-watch update join `events.watch_id`, already on the inbox query's join
path; the per-user indexes narrow the scan first.

## Open questions

- (none)

## Decisions

- Two ways to acknowledge a watch, chosen by babs on 2026-11-20: the filter's select all covers the
  loaded page, as the inbox does today; the per-watch action covers every unread item on the server.
- The per-watch action is "read" only, with no confirmation and no undo: marking read loses nothing, and
  the items stay in All.
- Archived items are left out of the per-watch action, like the Unread tab and the unread count.
- The parameter is named `watch`, like the `event` parameter the inbox URL already takes: short URLs.
- A watch the caller cannot read gives an empty list (or `updated: 0`), not a 404: the inbox query already
  hides such items, and this leaks nothing about other users' watches.
- The filter keeps the current tab and priority toggle: the noisy-watch flow happens in Unread.

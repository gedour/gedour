# 002 — Sign-in gate

**Status**: shipped
**Requested by**: babs
**Date**: 2026-11-04

## Problem

Without a session the app shell renders first and its pages fire API calls; the first 401 sends the
browser straight to the identity provider. When that redirect is suppressed (the 15 s loop guard) or
the provider is down, the user sits in a half-loaded shell full of errors, or on the provider's own
error page. A session that expires in the background (idle timeout, revoked, provider outage) is only
noticed when some call happens to fail.

## Solution

Nothing of the app renders until the session is confirmed. Without one, a full-screen sign-in gate
shows the product name and a "Sign in" button. A session lost later, whether through an API call, the
live inbox stream or a mutation, replaces the whole app with the same gate, keeping the current page as
the return target. If the identity provider cannot be reached, the gate says so and offers a retry
instead of leaving the user on an error page.

## Scope

- Session check (`/api/v1/me`) before the shell renders; a neutral full-screen loading state meanwhile.
- One gate component, reached from: no session at boot, any 401 from `web/src/api`, the inbox stream's
  `signed_out` event, and a stream error followed by a failed session check.
- "Sign in" navigates to `/auth/login?return_to=<current path>`.
- Provider unreachable: `/auth/login` and `/auth/callback` redirect to the gate with a reason instead of
  answering a plain-text error; the gate shows "Sign-in service unavailable" and a retry.
- The 15 s redirect loop guard and its `gedour.login.redirectedAt` key go away (no more automatic
  redirect to loop on).
- Dev single-user mode keeps signing in on its own: the gate never shows there.

## Out of scope

- Silent token refresh or re-authentication in an iframe.
- Keeping unsaved form input across the sign-in round trip.
- `/auth/signed-out` (explicit sign-out) stays as is.

## Acceptance criteria

- [x] **AC1** Given no session, when any app URL loads, then only the sign-in gate renders (no sidebar,
  no page content) and no API call other than `/api/v1/me` (and `/api/v1/version`) is made.
- [x] **AC2** Given the gate, when "Sign in" is clicked, then the browser goes to
  `/auth/login?return_to=<the URL first requested>`, and after a successful login lands back on it.
- [x] **AC3** Given a signed-in user, when the session ends server-side and the next API call answers
  401, then the app is replaced by the gate within that call; no error toast or broken view is shown.
- [x] **AC4** Given a signed-in user on any page, when the inbox stream sends `signed_out`, or errors and
  `/api/v1/me` then answers 401, then the gate replaces the app without user action.
- [x] **AC5** Given the identity provider unreachable, when "Sign in" is clicked, then the gate comes
  back with "Sign-in service unavailable" and a working retry, never a plain-text error page.
- [x] **AC6** A failed callback (state mismatch, provider error, expired login cookie) lands on the gate
  with "Sign-in failed, try again", not on a plain-text error.
- [x] **AC7** In dev single-user mode the gate never renders; the app signs in as before.
- [x] **AC8** The gate passes the existing a11y (axe, light and dark), 375 px and keyboard checks, and
  the e2e CSP guard.

## Phases

### Phase 1 — Gate and central 401 handling
- Work: session boot check, gate component, 401 and stream hooks into one auth state, loop-guard
  removal, server-side login/callback error redirects with a reason, unit + e2e tests, docs (AGENTS.md
  persisted keys, README auth section).
- **Covers**: AC1, AC2, AC3, AC4, AC5, AC6, AC7, AC8
- **Data model impact**: none.
- **DoD**: `make coverage` and `make e2e` green with new specs for expired session, stream sign-out and
  provider down; deployed on the lab, the gate shows on a fresh browser and after the session cookie is
  deleted.

## Data model impact (summary)

None.

## Open questions

- [ ] None.

## Decisions

- No automatic redirect at boot: the gate always asks for the click. A user with a live provider
  session pays one click; in exchange a provider outage never traps the browser. Overturnable.
- Reasons travel as a fixed query value (`?auth=unavailable|failed`) on the gate route, never as
  provider text, so nothing user-controlled is rendered.

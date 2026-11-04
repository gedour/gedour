# 001 — gedour v0

**Status**: in progress
**Requested by**: babs
**Date**: 2026-11-04

## Problem

Following upstream activity (new releases, image tags, the state of a GitHub PR or GitLab MR) means
polling pages by hand or stacking per-forge notification settings that are noisy and scattered. There
is no single place that tracks the last known state of each thing watched, says only what changed,
filters out the noise and pushes the rest to the right channel, with a trace of who changed what.

## Solution

gedour is a self-hosted watch service. A signed-in user declares watches (a GitHub repo's releases, a
GitLab MR, a Docker image's tags...). gedour polls each one on its schedule, keeps its last state and
turns every change into an event. A filter written as a one-line expression decides which events
matter. Matching events land in the user's inbox and go to the channels they picked: Telegram, email,
an outgoing webhook, or a JS script that does whatever is needed. Watches can be shared with other
users. Every change to configuration is in an audit log. The web UI and the public JSON API do the same
things; the UI is a client of the API.

## Scope

- Go single binary, roles selectable by flag (`web`, `worker`, both by default). PostgreSQL is the
  only dependency: data, job queue, scheduling locks, live-update fan-out.
- Runs under docker compose (dev, single host) and Kubernetes (several replicas per role).
- Auth: OIDC (authorization code + PKCE). A dev single-user mode for local runs and tests.
- Users own watches, channels and scripts; a watch can be shared read-only or read-write with other
  users.
- Watch kinds:
  - `github_release`: releases of a repo (tag, name, prerelease, draft, published_at).
  - `github_pr`: state of one PR (open, draft, merged, closed, review decision, CI status).
  - `gitlab_release`: releases of a project (gitlab.com or self-hosted).
  - `gitlab_mr`: state of one MR (opened, draft, merged, closed, approvals, pipeline status).
  - `oci_tags`: tags of an image on any OCI distribution registry (Docker Hub, GHCR, Quay, ...),
    with an optional tag regex and semver ordering.
- Credentials (GitHub/GitLab tokens, registry auth) stored encrypted, attached to watches.
- Scheduling: per-watch interval or cron expression, jitter, exponential backoff on errors.
- Change detection: normalized state snapshot per watch; a diff produces typed events
  (`release.published`, `tag.created`, `pr.state_changed`, ...).
- Filters: `expr-lang/expr` expressions evaluated against the event, validated on save.
- Routing rules: watch + filter → one or more channels.
- Channels: inbox (always on), Telegram, email (SMTP), outgoing webhook (HMAC-signed), JS script
  (goja, sandboxed).
- Deliveries with retries and a visible per-delivery status.
- Inbox: per user, unread/read/archived, live updates in the UI.
- Audit log: append-only, every mutation, viewable and filterable in the UI.
- REST JSON API with an OpenAPI document; the SPA uses only this API.
- UI (React + TypeScript, built with Vite, embedded in the binary): collapsible and resizable sidebar
  with persisted width and state, light/dark theme, keyboard shortcuts, empty/loading/error states on
  every view, toasts, responsive down to mobile width, keyboard navigable.
- Tests and coverage gates: Go unit + PG integration, frontend unit, Playwright end-to-end, coverage
  floors enforced in CI.
- Ops: `/healthz`, Prometheus `/metrics` on a separate port, structured logs, Dockerfile, compose file,
  Kubernetes manifests, GitHub Actions CI.

## Out of scope

- API keys / personal access tokens for the API (auth layer is built so they slot in later).
- Incoming webhooks from GitHub/GitLab (push instead of poll).
- Other sources: RSS, generic HTTP/JSON, custom JS collectors.
- Other channels: ntfy, Gotify, Teams, Slack, Discord.
- Roles beyond owner/shared (no admin console, no groups/teams).
- Digests and quiet hours.
- Multi-tenancy across organisations.
- Helm chart (plain Kustomize manifests only).

## Acceptance criteria

Auth and users

- [ ] **AC1** With OIDC configured, an unauthenticated request to any `/api/*` route returns 401 and
  any UI route redirects to the provider login; after login a user row exists with the OIDC `sub`.
- [ ] **AC2** With `GEDOUR_DEV_USER` set and OIDC unset, every request is authenticated as that user;
  the binary refuses to start if both are set, and logs a `dev_mode_enabled` warning at startup.
- [ ] **AC3** Session cookies are `HttpOnly`, `Secure` (except dev mode), `SameSite=Lax`; state-changing
  API calls without a valid CSRF token return 403.

Watches and sharing

- [ ] **AC4** A user can create, edit, pause, resume and delete a watch of each kind through the API
  and the UI; invalid config (unknown repo format, bad regex, bad cron) is rejected with 400 and a
  field-level error shown in the form.
- [ ] **AC5** A user cannot read or modify another user's watch unless it is shared with them (404,
  not 403, to avoid leaking existence); read-only shares return 403 on write.
- [ ] **AC6** Credentials are stored encrypted (AES-GCM, key from env); the API never returns a secret
  value, only its name and last 4 characters.

Collection and events

- [ ] **AC7** For each of the 5 kinds, a collector test against a recorded fixture produces the expected
  normalized state, and a second run with a changed fixture produces exactly the expected events.
- [ ] **AC8** A first run on a new watch records the baseline state and emits no event.
- [ ] **AC9** With 2 worker replicas against the same PG, each due watch runs exactly once per due
  time (integration test with 50 watches over 10 ticks).
- [ ] **AC10** A failing collector moves the watch to an error state with the last error message,
  retries with exponential backoff capped at the watch interval x 8, and recovers on success.
- [ ] **AC11** GitHub/GitLab/registry rate limit responses delay the next run until the reset time
  instead of hammering; conditional requests (ETag) are used where the API supports them.
- [ ] **AC12** `oci_tags` works anonymously on Docker Hub and GHCR public images and with credentials
  on a private registry; `tag_regex` and semver ordering filter the tags tracked.

Filters, rules, channels

- [ ] **AC13** A filter expression is compiled on save; a syntax or type error returns 400 with the
  position; the UI offers a "test against last 20 events" preview.
- [ ] **AC14** An event matching a rule creates one inbox item and one delivery per channel of the
  rule; a non-matching event creates neither.
- [ ] **AC15** Telegram, SMTP and webhook deliveries retry up to 5 times with backoff; the delivery
  list shows status, attempt count and last error.
- [ ] **AC16** Outgoing webhooks carry an `X-Gedour-Signature` HMAC-SHA256 header over the body;
  targets resolving to loopback, link-local or private ranges are refused unless allowlisted in
  config.
- [ ] **AC17** A JS script channel receives the event, can call an injected `http.fetch` (same SSRF
  rules as AC16) and `log`; it is killed after 5 s and has no filesystem, process or env access.
- [ ] **AC18** The UI script editor (CodeMirror) highlights JS, and a "run with sample event" button
  shows the script's logs and result without sending anything for real.

Inbox and audit

- [ ] **AC19** A new inbox item appears in an open UI within 2 s without reload (SSE fed by PG
  `LISTEN/NOTIFY`), on any web replica.
- [ ] **AC20** Inbox items can be marked read/unread, archived, bulk-actioned; the sidebar shows the
  unread count.
- [ ] **AC21** Every create/update/delete on watches, shares, credentials, rules, channels and scripts
  writes an audit row (actor, action, target, before/after JSON with secrets redacted) in the same
  transaction; a failed audit write rolls the mutation back.
- [ ] **AC22** The audit view lists entries newest first, filterable by actor, target type and date.

UX

- [ ] **AC23** The sidebar collapses to icons and expands back by button and shortcut; its width is
  resizable by drag between 180 and 480 px; width and collapsed state survive a reload.
- [ ] **AC24** Every list view has distinct empty, loading and error states (Playwright asserts each).
- [ ] **AC25** Light/dark theme follows the OS by default and can be forced; the choice persists.
- [ ] **AC26** All main flows are usable at 375 px width (sidebar becomes a drawer) and by keyboard
  only; axe-core reports no serious or critical violation on each main view.

Ops and quality

- [ ] **AC27** `/healthz` returns 200 when PG is reachable, 503 otherwise; `/metrics` on the metrics
  port exposes run counts, run durations, event counts and delivery outcomes per kind/channel.
- [ ] **AC28** `docker compose up` brings up PG + gedour in dev mode with a working UI on a clean
  machine; `kubectl apply -k deploy/k8s` deploys 2 web + 2 worker replicas.
- [ ] **AC29** CI runs lint, Go tests (with PG), frontend tests, Playwright e2e and the image build on
  every PR; it fails if Go coverage < 80 % or frontend coverage < 80 % (lines).
- [ ] **AC30** The OpenAPI document is served at `/api/openapi.json` and a CI check fails if the
  handlers and the document drift.

## Phases

### Phase 1 — Skeleton and quality gates
- Work: Go module via `go-init` standard (config, zap, dual servers, graceful shutdown, `/healthz`,
  `/metrics`), PG connection + migrations (goose, embedded), roles flag, Vite + React + TS app embedded
  via `embed`, app shell with sidebar, theme, routing, Dockerfile, compose, Makefile, golangci-lint,
  eslint, pre-commit, GitHub Actions CI with coverage gates, Playwright harness.
- **Covers**: AC23, AC25, AC27, AC29
- **Data model impact**: migrations framework only, no business tables.
- **DoD**: `make test` and `make e2e` green locally; CI green on the PR; `docker compose up` serves the
  shell at `http://localhost:8080`; coverage report produced for Go and frontend.

### Phase 2 — Auth, users, audit core
- Work: OIDC login/logout/callback, PG-backed sessions, CSRF, dev single-user mode, current-user
  endpoint, audit log writer (transactional helper) and audit view.
- **Covers**: AC1, AC2, AC3, AC21, AC22
- **Data model impact**: `users`, `sessions`, `audit_log`.
- **DoD**: Go integration tests against an in-process OIDC stub pass; Playwright logs in through a
  Keycloak container (realm imported from `deploy/keycloak/realm.json`, also used by the compose
  `oidc` profile); audit rollback test passes.

### Phase 3 — Watches, credentials, sharing
- Work: CRUD API + OpenAPI for watches and credentials, encryption at rest, shares, list/detail/form
  views with validation, empty/loading/error states.
- **Covers**: AC4, AC5, AC6, AC24, AC30
- **Data model impact**: `watches`, `credentials`, `watch_shares`.
- **DoD**: API tests cover ownership matrix (owner, ro share, rw share, stranger); OpenAPI drift check
  in CI; Playwright creates/edits/deletes a watch of each kind.

### Phase 4 — Scheduler, collectors, events
- Work: PG queue (`next_run_at` + `FOR UPDATE SKIP LOCKED`), backoff, rate-limit handling, ETag cache,
  the 5 collectors, state store, diff → events, watch detail shows state history and events.
- **Covers**: AC7, AC8, AC9, AC10, AC11, AC12
- **Data model impact**: `watch_states`, `events`, scheduling columns on `watches`.
- **DoD**: fixture tests per kind green; 2-worker exactly-once integration test green; manual run
  against real `github.com/golang/go` releases and `docker.io/library/postgres` tags shows events.

### Phase 5 — Filters, rules, channels, inbox
- Work: expr filters with preview, rules, channels inbox/Telegram/SMTP/webhook, delivery worker with
  retries, SSRF guard, SSE live inbox, unread count.
- **Covers**: AC13, AC14, AC15, AC16, AC19, AC20
- **Data model impact**: `rules`, `rule_channels`, `channels`, `deliveries`, `inbox_items`.
- **DoD**: delivery tests against httptest servers (webhook, Telegram API stub) and a local SMTP sink
  (mailpit in compose); SSE test across 2 web replicas.

### Phase 6 — Script callbacks
- Work: goja runtime with timeout, injected `http.fetch`/`log`, script channel, CodeMirror editor with
  dry-run.
- **Covers**: AC17, AC18
- **Data model impact**: `scripts`.
- **DoD**: sandbox escape tests (fs, env, infinite loop, SSRF) all fail safely; Playwright edits and
  dry-runs a script.

### Phase 7 — Kubernetes and hardening
- Work: Kustomize manifests (web + worker Deployments, Service, PDB, probes, secrets refs), a11y pass,
  mobile drawer, keyboard pass. Lab deployment on `lab-cluster`: ArgoCD app in the `gitops-repo`
  app-of-apps, database as a `shared-pg` CNPG tenant, secrets through External Secrets
  (`lab-secret-store`), private ingress `https://gedour.lab.example` (class `private-ingress`,
  LAN only), OIDC client in the lab Keycloak (`idp.lab.example`, realm `apps`).
- **Covers**: AC26, AC28
- **Data model impact**: none.
- **DoD**: Argo app `gedour` Synced/Healthy with 2+2 replicas; login through `idp.lab.example` works on
  `https://gedour.lab.example`; axe-core Playwright job green; 375 px Playwright run green.

## Data model impact (summary)

New database. Tables: `users`, `sessions`, `audit_log`, `credentials`, `watches`, `watch_shares`,
`watch_states`, `events`, `channels`, `rules`, `rule_channels`, `deliveries`, `inbox_items`, `scripts`.

## Open questions

None.

## Decisions

- API-first REST + SPA (React, TS, Vite, pnpm) — because the API must exist for future API keys, so a
  server-rendered UI would mean two front doors to maintain.
- PostgreSQL as queue, lock and pub/sub — because it is already required and removes Redis/NATS.
- `expr-lang/expr` for filters, goja for scripts — expressions stay side-effect free, scripts get a
  sandbox with a hard timeout.
- Migrations with goose, embedded in the binary — run at startup by a single advisory-locked replica.
- Coverage floors 80 % Go and 80 % frontend lines — high enough to bite, low enough not to force tests
  on generated code.
- Deny-by-default SSRF guard on webhooks and script fetch — a user-supplied URL is a request from
  inside the cluster.
- Kustomize over Helm for v0 — one environment shape, no templating needed yet.
- Keycloak for e2e and the compose `oidc` profile, an in-process stub for Go integration tests —
  Keycloak proves the real flow, the stub keeps `go test` free of a 30 s container start.
- Images on GHCR `ghcr.io/gedour/gedour`, multi-arch, cosign keyless — per the GitHub Actions
  standard.
- Lab Keycloak realm `apps` (the broker realm) rather than `lab` — app clients belong on the
  broker; change if the client should sit on `lab`.
- Not-shared resources return 404 — avoids leaking which watch IDs exist.

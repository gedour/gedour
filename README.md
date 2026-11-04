# gedour

Self-hosted watch service: follows upstream releases, pull/merge requests and image tags, keeps their
last state, and sends only the changes that matter to the channels you pick.

Status: phase 4 (scheduler, collectors, events). Spec: `specs/001-gedour-v0.md`.

## Quickstart

```sh
make dev   # PostgreSQL + gedour via docker compose
```

UI on <http://localhost:8080> in dev single-user mode (signed in as `dev@local`), metrics on
<http://localhost:9090/metrics>.

```sh
docker compose --profile oidc up --build   # adds Keycloak on :8081 and gedour with OIDC on :8082
```

<http://localhost:8082> signs in through Keycloak's test realm (`alice`/`alice`, `bob`/`bob`,
`deploy/keycloak/realm.json`).

| Variable | Default | Purpose |
|---|---|---|
| `GEDOUR_KC_PORT` | `8081` | Keycloak port, the same inside and outside the container (it is in the issuer URL) |
| `GEDOUR_OIDC_PORT` | `8082` | Host port of the OIDC-mode gedour |
| `GEDOUR_DB_PORT` | `55432` | Host port of PostgreSQL |

The realm's redirect and post-logout URIs pin the app ports: `localhost:8082` for compose, `localhost:18080`
for `make e2e`. Changing `GEDOUR_OIDC_PORT` (or `E2E_PORT`) needs the same change in `realm.json`.

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `GEDOUR_DATABASE_URL` | (required) | PostgreSQL URL, e.g. `postgres://user:pass@host:5432/gedour` |
| `GEDOUR_ROLE` / `--role` | `all` | `web`, `worker` or `all` |
| `GEDOUR_HTTP_ADDR` | `:8080` | UI, `/api`, `/healthz` |
| `GEDOUR_METRICS_ADDR` | `:9090` | `/metrics`, `/healthz` |
| `GEDOUR_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `GEDOUR_LOG_FORMAT` | `auto` | `auto` (console on a TTY, else JSON), `console`, `json` |
| `GEDOUR_SHUTDOWN_TIMEOUT` | `15s` | Drain time on SIGTERM |
| `OIDC_ISSUER` | | OIDC provider issuer URL |
| `OIDC_CLIENT_ID` | | OIDC client ID (confidential client, authorization code + PKCE) |
| `OIDC_CLIENT_SECRET` | | OIDC client secret; also keys the short-lived login state cookie |
| `GEDOUR_PUBLIC_URL` | (required with OIDC) | Browser-facing base URL; the redirect URI is `<url>/auth/callback` |
| `GEDOUR_DEV_USER` | | Dev single-user mode: every request runs as this user. Answers `/api` and `/auth` only on a loopback Host (`localhost`, `127.0.0.1`, `::1`), others get 403. Never in production |
| `GEDOUR_SESSION_TTL` | `168h` | Absolute session lifetime |
| `GEDOUR_SESSION_IDLE_TIMEOUT` | `24h` | Session ends after this long without a request |
| `GEDOUR_ENCRYPTION_KEY` | (required) | Base64 of 32 random bytes (`openssl rand -base64 32`); AES-256-GCM key of stored credentials. Losing it makes them unreadable |
| `GEDOUR_ALLOW_HTTP_HOSTS` | | Comma-separated hosts a watch URL (`api_url`, `base_url`, an OCI `registry`) may reach over plain http; others need https |
| `GEDOUR_ALLOW_PRIVATE_NETS` | | Comma-separated CIDRs outbound requests may reach despite the SSRF guard, e.g. `10.20.0.0/16` for an internal registry |
| `GEDOUR_WORKER_POLL` | `2s` | How often an idle worker looks for due watches |
| `GEDOUR_WORKER_CONCURRENCY` | `8` | Runs one worker process executes at once |

The web role needs either the three `OIDC_*` variables plus `GEDOUR_PUBLIC_URL`, or `GEDOUR_DEV_USER`;
both or neither refuse to start. The session cookie is `__Host-gedour_session` (`Secure`) with OIDC, plain
`gedour_session` in dev mode: serve gedour over HTTPS
(browsers make an exception for `http://localhost` only).

OIDC client settings at the provider (Keycloak: confidential client, standard flow, PKCE S256):

- Valid redirect URIs: `<GEDOUR_PUBLIC_URL>/auth/callback`
- Valid post logout redirect URIs: `<GEDOUR_PUBLIC_URL>/auth/signed-out` (without it, sign-out stops on
  the provider's error page)

## Collection

- The worker role claims due watches from PostgreSQL (`FOR UPDATE SKIP LOCKED`, 5 min lease): replicas
  share the load, each due run executes once, and a crashed worker's watch runs again after the lease.
  Runs time out after 60 s (stored as an error, with backoff); on SIGTERM in-flight runs get
  `GEDOUR_SHUTDOWN_TIMEOUT` to finish. A run-now, resume or edit during a run never starts a second one:
  the watch reads `running: true`, and the request is kept for when the run ends.
- Next run: interval schedules ±10 % jitter, cron schedules at their next activation (UTC). Errors back
  off `interval × 2^(failures-1)`, capped at 8 intervals. Rate limits (GitHub `X-RateLimit-*`, GitLab
  `RateLimit-Reset`, `Retry-After`) push the next run to the reset time, at most 1 h away, without
  counting as a failure. Release watches send `If-None-Match`/`If-Modified-Since`; a 304 means unchanged.
- The first run stores a baseline and emits nothing. Each later change becomes typed events
  (`release.published`, `release.promoted`, `pr.state_changed`, `pr.draft_changed`, `pr.review_changed`,
  `pr.ci_changed`, `mr.state_changed`, `mr.draft_changed`, `mr.approval_changed`, `mr.pipeline_changed`,
  `tag.created`, `tag.removed`). Changing a watch's target clears its states: the next run is a new baseline.
  The last 50 states per watch are kept; events are kept.
- SSRF guard: outbound requests refuse loopback, private (RFC 1918, ULA), link-local (cloud metadata
  included), CGNAT, multicast and reserved addresses (IPv4 inside NAT64 and 6to4 included, Teredo refused),
  checked after DNS resolution on every connection,
  redirects included; a redirect to another host drops `Authorization` and `PRIVATE-TOKEN`. Environment
  proxies are ignored. `GEDOUR_ALLOW_PRIVATE_NETS` opens chosen ranges.
- Metrics: `gedour_collector_runs_total{kind,status}`, `gedour_collector_run_duration_seconds{kind}`,
  `gedour_events_total{kind,type}`, `gedour_watches_due`.

## Endpoints

- `GET /healthz`: 200 when PostgreSQL answers, 503 otherwise.
- `GET /api/openapi.json`: OpenAPI document.
- `GET /api/v1/version`: build information.
- `GET /auth/login?return_to=/path`, `GET /auth/callback`: sign-in flow.
- `POST /auth/logout` (with `X-CSRF-Token`): ends the session and answers `{"redirect": "<url>"}`, the
  provider's `end_session_endpoint` (so its session ends too), or `/auth/signed-out` when it has none.
- `GET /api/v1/me`: current user and the CSRF token to send as `X-CSRF-Token` on every non-GET call.
  A missing or stale token answers 403 problem+json with `title: "CSRF token invalid"`; refetch
  `/api/v1/me` and retry once.
- `GET /api/v1/audit`: audit entries, newest first (`target_type`, `actor`, `from`, `to`, `cursor`, `limit`).
  A user sees what they did, plus entries on watches they can read now and on their credentials; share
  entries go to the watch owner and, for their own share only, to the sharee. `GET /api/v1/audit/actors`: the actors of those entries.
- `/api/v1/watches` (`GET ?kind=&q=&shared=mine|with_me|all`, `POST`), `/api/v1/watches/{id}` (`GET`,
  `PATCH`, `DELETE`), `POST /api/v1/watches/{id}/pause|resume`. A watch carries its run status:
  `next_run_at`, `running` (true while a worker holds the claim), `last_run_at`, `last_status` (`ok`, `error`,
  `rate_limited`), `last_error`, `consecutive_failures`.
- `GET /api/v1/watches/{id}/events?cursor=&limit=`: events, newest first. `GET /api/v1/watches/{id}/states?limit=`:
  state history, latest first. `POST /api/v1/watches/{id}/run` (read-write share or owner, 409 when paused):
  runs at the next poll. Resume also runs at once.
- `/api/v1/watches/{id}/shares` (`GET`, `POST {email, permission}`), `/api/v1/watches/{id}/shares/{user_id}`
  (`PUT {permission}`, `DELETE`). Shares are `ro` (read) or `rw` (edit, pause; not credential, sharing or
  delete; not the config of a watch that uses the owner's credential). A sharee lists only their own
  share. A watch the caller cannot read answers 404, one they cannot change 403.
- `/api/v1/credentials` (`GET`, `POST`), `/api/v1/credentials/{id}` (`GET`, `DELETE`, 409 while a watch uses
  it), `PUT /api/v1/credentials/{id}/secret`. Secrets are write-only; responses carry `last4` (empty for
  secrets under 16 characters). Credentials are private to their owner.

The full contract is `api/openapi.json` (also served at `/api/openapi.json`).

Every other `/api` route needs a session (401 otherwise).

## Development

See `AGENTS.md` for build, test, lint and e2e commands.

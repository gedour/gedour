# gedour

Self-hosted watch service: follows upstream releases, pull/merge requests and image tags, keeps their
last state, and sends only the changes that matter to the channels you pick.

Status: phase 2 (auth, users, audit). Spec: `specs/001-gedour-v0.md`.

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

The web role needs either the three `OIDC_*` variables plus `GEDOUR_PUBLIC_URL`, or `GEDOUR_DEV_USER`;
both or neither refuse to start. The session cookie is `__Host-gedour_session` (`Secure`) with OIDC, plain
`gedour_session` in dev mode: serve gedour over HTTPS
(browsers make an exception for `http://localhost` only).

OIDC client settings at the provider (Keycloak: confidential client, standard flow, PKCE S256):

- Valid redirect URIs: `<GEDOUR_PUBLIC_URL>/auth/callback`
- Valid post logout redirect URIs: `<GEDOUR_PUBLIC_URL>/auth/signed-out` (without it, sign-out stops on
  the provider's error page)

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

Every other `/api` route needs a session (401 otherwise).

## Development

See `AGENTS.md` for build, test, lint and e2e commands.

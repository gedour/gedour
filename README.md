# gedour

Self-hosted watch service: follows upstream releases, pull/merge requests and image tags, keeps their
last state, and sends only the changes that matter to the channels you pick.

Status: phase 1 (skeleton). Spec: `specs/001-gedour-v0.md`.

## Quickstart

```sh
make dev   # PostgreSQL + gedour via docker compose
```

UI on <http://localhost:8080>, metrics on <http://localhost:9090/metrics>.

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

## Endpoints

- `GET /healthz`: 200 when PostgreSQL answers, 503 otherwise.
- `GET /api/openapi.json`: OpenAPI document.
- `GET /api/v1/version`: build information.

## Development

See `AGENTS.md` for build, test, lint and e2e commands.

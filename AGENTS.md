# AGENTS.md

## Setup

- Go 1.27, Node 24 + pnpm (version pinned in `web/package.json` `packageManager`), Docker (tests start PostgreSQL via testcontainers).
- `pnpm --dir web install --frozen-lockfile` once; `make` targets re-run it when the lockfile changes.
- JS packages: pnpm only. Build scripts are refused unless listed in `web/pnpm-workspace.yaml` `allowBuilds`.

## Commands

| Task | Command |
|---|---|
| Build UI then binary (`bin/gedour`) | `make build` |
| Go + web unit tests | `make test` |
| Coverage gates (Go and web, 80 % each) | `make coverage` |
| Lint (golangci-lint, eslint, tsc, prettier) | `make lint` |
| Playwright e2e (real binary + compose PostgreSQL) | `make e2e` |
| Dev stack on :8080 / :9090 | `make dev` |
| Pre-commit | `pre-commit run --all-files` |

- Go DB tests fail when Docker is down; `GEDOUR_SKIP_DB_TESTS=1` turns that into skips (never in CI).
- `make e2e` ports: `E2E_PORT`, `E2E_METRICS_PORT`, `E2E_DB_PORT` (defaults 18080, 19090, 55433).

## Go tests without a frontend build

`web/embed.go` embeds `web/dist`, which only holds a committed `.gitkeep` on a fresh clone.
`go test ./...` and `go build` work without building the UI; the binary then answers `/` with 503
"frontend not built". `pnpm build` re-creates `dist/.gitkeep` after Vite empties the directory: keep it.
`go.mod` has `ignore ./web/node_modules` so `./...` never walks into npm packages.

## Conventions

- Layout: `cmd/gedour` (flags, logger, signals only), logic in `pkg/<domain>`; coverage is measured on `./pkg/...`.
- `/api/*` is huma (OpenAPI generated from handlers, served at `/api/openapi.json`). Unknown `/api` paths
  answer problem+json 404, known paths with a wrong method problem+json 405 + `Allow`; never the SPA.
  Other GET paths fall back to `index.html`.
- `/metrics` lives only on the metrics listener; `/healthz` is on both listeners.
- The main listener sends a CSP whose `script-src` hashes are computed at startup from the served
  `index.html`. No `<style>` elements or `style=""` attributes in markup, no data: URIs (Vite
  `assetsInlineLimit: 0`); CSSOM writes (React `style` prop) are fine. Radix menus run with
  `modal={false}` (modal mode injects a `<style>`); e2e fails on any CSP violation.
- `index.html`'s pre-paint theme script reads `%GEDOUR_THEME_KEY%`, filled by Vite from `src/lib/theme.ts`.
- CI jobs live in the reusable `.github/workflows/checks.yml`, called by `ci.yml` and gating `release.yml`.
  Actions are pinned to commit SHAs with a `# vX.Y.Z` comment; Dependabot bumps them.
- Migrations: goose SQL files in `pkg/db/migrations`, embedded, applied at startup under a PG advisory lock.
  Never edit an applied migration; add a new numbered file.
- Version vars are injected with `-X 'main.<Var>=…'` (never the import path).
- Frontend: `fetch` only in `web/src/api/`; UI primitives in `web/src/components/ui` (shadcn-style, Radix).
- Persisted UI keys: `gedour.theme`, `gedour.sidebar.width`, `gedour.sidebar.collapsed`.

## Acceptance-criteria citations

Every test written for a spec acceptance criterion cites it in a comment as `001-ACn`
(spec number, then AC number), e.g. `// 001-AC23: ...`. Grep `001-AC` to find the coverage of an AC.

## Sensitive data

- `.env` holds real secrets: never read, print or commit it. It is git- and docker-ignored.

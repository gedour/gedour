# AGENTS.md

## Setup

- Go 1.27, Node 26 + pnpm (version pinned in `web/package.json` `packageManager`), Docker (tests start PostgreSQL via testcontainers).
- `pnpm --dir web install --frozen-lockfile` once; `make` targets re-run it when the lockfile changes.
- JS packages: pnpm only. Build scripts are refused unless listed in `web/pnpm-workspace.yaml` `allowBuilds`.

## Commands

| Task | Command |
|---|---|
| Build UI then binary (`bin/gedour`) | `make build` |
| Go + web unit tests | `make test` |
| Coverage gates (Go and web, 80 % each) | `make coverage` |
| Lint (golangci-lint, eslint, tsc, prettier) | `make lint` |
| Regenerate `api/openapi.json` and `web/src/api/schema.gen.ts` | `make openapi` |
| Playwright e2e (real binary + compose PostgreSQL) | `make e2e` |
| kind + compose smoke (001-AC28), needs kind with a Kubernetes 1.30+ node (`KIND=<path>`) | `make k8s-smoke` |
| Dev stack on :8080 / :9090 | `make dev` |
| Pre-commit | `pre-commit run --all-files` |

- Go DB tests fail when Docker is down; `GEDOUR_SKIP_DB_TESTS=1` turns that into skips (never in CI).
- `make e2e` signs in through Keycloak (compose `oidc` profile, `deploy/keycloak/realm.json`) as `alice`;
  ports `E2E_PORT` (18080), `E2E_METRICS_PORT`, `E2E_DB_PORT`,
  `E2E_KC_PORT`, `E2E_OIDC_APP_PORT`, `E2E_UPSTREAM_PORT` (`cmd/fakeupstream`, `POST /_fixture/a|b` switches it),
  `E2E_SMTP_PORT`/`E2E_MAILPIT_PORT` (Mailpit). The base URL is `localhost`, never `127.0.0.1` (cookie origin).
- Parallel `make e2e`: one worktree per run, each with its own `E2E_PROJECT` (compose project, default `gedour-e2e`) and all these ports.
- The fake upstream also serves Telegram (`/tg/bot<token>/sendMessage`) and webhook receivers
  (`POST /_hooks/<name>`, `fail*` answers 500); `GET /_received` lists what they got. Its Talk endpoint checks
  the signature against `testupstream.TalkSecret`; tokens `force404*`/`force429*` force that status.
- The fixture set is global: only `activity.spec.ts` (serial) and `seed.setup.ts` switch it. The `seed` project
  runs before the parallel specs and leaves 3 events in bob's inbox, 2 of them high (005). The a11y, 375 px, keyboard,
  priority, push, api-keys and mcp specs run as bob (channels and watches in part). Specs open (mark read) the newest
  or a high item, only the keyboard spec touches the rc promotion item; a11y, 375 px and keyboard add push devices for bob.
- Code collecting response bodies must skip `/api/v1/inbox/stream` (SSE, never ends).
- Notifications need `test.use({ channel: "chromium" })`: the default headless shell reports them denied
  even when granted (`push.spec.ts`, which also stubs `PushManager.subscribe` to point at the fake push service).
- Clear-text SMTP (`GEDOUR_SMTP_TLS=none`) is opened by `GEDOUR_SMTP_PLAIN_NETS`, never by
  `GEDOUR_ALLOW_PRIVATE_NETS` (that one also opens webhook and Nextcloud Talk targets).
- Playwright specs import `test` from `e2e/fixtures.ts` (CSP guard); the `setup` project signs alice and
  bob in from the gate (`signInFromGate`) and stores their sessions in `web/playwright/.auth/`.

## Go tests without a frontend build

`web/embed.go` embeds `web/dist`, which only holds a committed `.gitkeep` on a fresh clone.
`go test ./...` and `go build` work without building the UI; the binary then answers `/` with 503
"frontend not built". `pnpm build` re-creates `dist/.gitkeep` after Vite empties the directory: keep it.
`go.mod` has `ignore ./web/node_modules` so `./...` never walks into npm packages.

## Conventions

- Layout: `cmd/gedour` (flags, logger, signals only), logic in `pkg/<domain>`; coverage is measured on `./pkg/...`.
- `/api/*` is huma (OpenAPI generated from handlers, served at `/api/openapi.json`). Changing an operation
  needs `make openapi`: `TestOpenAPIDrift` (Go) and `pnpm check:api` (web) fail on a stale file. Huma ignores
  embedded structs in inputs: declare path params and body fields inline. Unknown `/api` paths
  answer problem+json 404, known paths with a wrong method problem+json 405 + `Allow`; never the SPA.
  Other GET paths fall back to `index.html`, except `/mcp`, `/mcp/`, `/.well-known/` and `/sw.js` (`web/public/sw.js`, 404 when missing).
- `/metrics` and `/livez` (liveness, ignores PG) live only on the metrics listener; `/healthz` is on both.
- The main listener sends a CSP whose `script-src` hashes are computed at startup from the served
  `index.html`. No `<style>` elements or `style=""` attributes in markup, no data: URIs (Vite
  `assetsInlineLimit: 0`); CSSOM writes (React `style` prop) are fine. Radix menus run with
  `modal={false}` (modal mode injects a `<style>`); modals use the native `<dialog>` of
  `components/ui/dialog.tsx`, never Radix Dialog/AlertDialog. e2e fails on any CSP violation.
- `index.html`'s pre-paint theme script reads `%GEDOUR_THEME_KEY%`, filled by Vite from `src/lib/theme.ts`.
- Default branch `develop`; releases are `vX.Y.Z` tags on it. CI jobs live in the reusable
  `.github/workflows/checks.yml`, called by `ci.yml` (PRs) and gating `release.yml` (tags and `develop`
  pushes, which publish `develop-<sha>-<run>` images).
  Actions are pinned to commit SHAs with a `# vX.Y.Z` comment; Dependabot bumps them.
- Migrations: goose SQL files in `pkg/db/migrations`, embedded, applied at startup under a PG advisory lock.
  Each lock wait stops at `lock_timeout` 5 s (`migrationLockTimeout`), then the pod restart retries. A large index build goes in its own NO TRANSACTION file with `CONCURRENTLY`, after a `DROP INDEX CONCURRENTLY IF EXISTS` for the retry; its wait on older transactions, on any table, counts too (`SET lock_timeout = 0` then `RESET lock_timeout` around it if it must outwait them).
  Never edit an applied migration; add a new numbered file.
- Version vars are injected with `-X 'main.<Var>=…'` (never the import path).
- Frontend: `fetch` only in `web/src/api/`; API resource types come from `schema.gen.ts` (via `types/api.ts`); UI primitives in `web/src/components/ui` (shadcn-style, Radix).
- A `lazy` route wraps its import in `ChunkLoadError` and sets `errorElement: <RouteError />` (`ScriptFormRoute`): reloads while the page stays open and `/healthz` answers, then a message (`e2e/chunk-recovery.spec.ts`).
- Persisted UI keys: `gedour.theme`, `gedour.sidebar.width`, `gedour.sidebar.collapsed`; `gedour.assetReloads` and `gedour.chunkReloads` in `sessionStorage`.
- One auth state: a 401 from `request()` or the stream's `signed_out` sets `auth` (`api/client.ts`) and
  `AuthGate` swaps the app for `SignInGate`. Only `api/client.ts` may navigate by script; app code
  signs out with `signOut()` and reloads with `reloadPage()`, `navigation` is a test seam (ESLint `no-restricted-*`, see `src/lint.test.ts`).
  One exception out of ESLint's reach: the reload in `index.html` when a hashed asset fails before `window.load`
  (`e2e/asset-recovery.spec.ts`). `renderApp(path, me)` starts signed in as `me`; `null` boots through the gate.
- Auth: `auth.Guard` puts every `/api` path behind a session, except those in its `publicAPI` set; mutations
  need `X-CSRF-Token`. Frontend calls go through `web/src/api/client.ts`, which adds it.
- `/auth/signed-out` is an SPA route outside the shell and the gate that must never call the API.
- Mutations run in `db.InTx` and call `audit.Record` with the same tx (001-AC21). Huma validation errors are 400.
- `db.InTx` retries a deadlock (40P01) once: its closure may run twice, so it resets what it captures and leaves for after commit the side effects that must happen only once. Any test with `testpg.NewDB` fails on a retry it did not allow (`testpg.AllowDeadlockRetries`).
- Row locks (`FOR UPDATE`/`NO KEY UPDATE`/`SHARE`) go users, rules, watches, credentials, by id; sessions and API keys come after users
  (act-as start takes its target `FOR SHARE` first). Purges serialize on an advisory lock taken first. Users are never locked `FOR UPDATE`: disable and purge
  take `NO KEY UPDATE` (sign-in, caps, transfer and act-as start still wait on them), so FK KEY SHARE on users (audit
  actor and on_behalf_of, sessions, router) never waits on them and may come anywhere. Events: router
  (event, then KEY SHARE on rule/channel/user) and deletes only; events are deleted explicitly before their watch (no reliance on cascade order), and pre-delete rule locks are `NO KEY UPDATE`.
  Channels: the router takes KEY SHARE on digest channels before inserting deliveries; the digest claim takes the channel
  `FOR NO KEY UPDATE SKIP LOCKED`, then its deliveries `SKIP LOCKED` too: it never waits under the channel lock (purge, TestPurgeRacingDigestClaim).
- `audit.Record` stores the value's `encoding/json` form; the key deny-list is only a safety net. A type
  passed to it must not carry secrets in its JSON form (`json:"-"`, or pass the API response type).

## MCP

- A tool is one row of `rows` in `pkg/mcpserver/tools.go`: name, operation ID, scope, description, limit caps, output shaper. Its data
  path is the REST handler, dispatched in process on the mux below `Guard`: no second code path, so a tool's checks live in the handler.
- `inputSchema` comes from `api.OpenAPI()`. `mcpserver.New` fails (`server.New` panics) on a row naming an unknown operation or a path
  under `auth.AdminPrefix`, as the sub-request skips `Guard`'s admin check.
- Tests sending several calls per key mind the burst of 20 (`keyBurst`).
- SDK code runs on its own goroutines (`jsonrpc2.handleAsync`, `readIncoming`), out of `net/http`'s recover. `wrap` guards what is below it; a panic ahead of it or behind it ends the process. Keep `servedMethods` narrow, and a new shape the source says could panic goes in `TestMCPOddRequestsSurvive`.
- A write tool's audit row gets `api_key_id` from `dispatch` (`audit.WithAPIKey`). Budget-spending rows set `limited`. `audit.APIKeyID` says a request came with a key, for checks only a key must pass.
- A row's `hidden` inputs leave its schema, and a handler refuses them too: `createWatch` and `updateWatch` hold the key's credential, retarget, known-host, cap and rate-limit-hold checks. Compare a target, host and path, with `comparableTarget`, never with `strings.ToLower` or a suffix: U+0130 and U+212A fold to ASCII.
- An authenticated `/mcp` line that must never drop goes in `unsampled` (`cmd/gedour/main.go`); anything reachable without a key stays sampled.

## Kubernetes

- Base in `deploy/k8s/base` (`deploy/k8s` only includes it): kustomize refuses an overlay nested in the
  directory it includes, so `deploy/k8s/test` (smoke only: dev mode, public key) points at `../base`.
- The image has no shell: no `exec` probes or `sh -c` hooks; use `httpGet` and the `sleep` action.
- A release tag must equal `newTag` in `deploy/k8s/base` and the README pins: bump them before tagging
  (`scripts/check-release-pin.sh <tag>`, its `--self-test` runs in CI).

## Collectors

- Recorded upstream fixtures live in `internal/testupstream/fixtures` (`<kind>_a.json`, `<kind>_b.json`), shared
  by the collector tests and the e2e fake. A changed collector needs both fixtures and the expected events updated.
- Release notes never reach `watch_states`: `Fetcher.Notes` carries them for one run, `WithNotes` adds them to
  the events. Fixtures: `cli/cli` fires 3 events a→b (CVEs, breaking change, both), its routine and no-notes
  releases stay in the baseline; `gitlab-runner` fires all 5 cases.
- e2e watches must target `E2E_UPSTREAM_URL`: `scripts/e2e.sh` fails when a run log lists a real forge host.
- Collector HTTP goes through the `pkg/netguard` client: tests against `httptest` (127.0.0.1) allowlist
  `127.0.0.0/8` and list `127.0.0.1` as a plain-http host.

## Script sandbox

- Scripts run in a child process: the running binary with argument `sandbox`. A test package that runs
  scripts calls `script.MaybeSandbox()` first in its `TestMain` and builds the runner from
  `os.Executable()`. Coverage of `pkg/script` comes from its in-process tests (`Serve` over pipes); the
  child's `Main` (rlimits, watchdog) only runs in subprocesses and is not measured.
- CodeMirror's `style-mod` is patched (`web/patches/`, `pnpm-workspace.yaml` `patchedDependencies`) to use a
  constructed stylesheet instead of a `<style>` element (CSP). A CodeMirror upgrade that bumps
  `style-mod` needs the patch redone; the e2e CSP guard fails without it.

## Acceptance-criteria citations

Every test written for a spec acceptance criterion cites it in a comment as `001-ACn`
(spec number, then AC number), e.g. `// 001-AC23: ...`. Grep `001-AC` to find the coverage of an AC.

## Sensitive data

- `.env` holds real secrets: never read, print or commit it. It is git- and docker-ignored.

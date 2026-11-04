# gedour

Self-hosted watch service: follows upstream releases, pull/merge requests and image tags, keeps their
last state, and sends only the changes that matter to the channels you pick.

Status: phase 6 (script callbacks). Spec: `specs/001-gedour-v0.md`.

## Quickstart

```sh
make dev   # PostgreSQL + gedour via docker compose
```

UI on <http://localhost:8080> in dev single-user mode (signed in as `dev@local`), metrics on
<http://localhost:9090/metrics>. Email channels go to Mailpit: <http://localhost:8025>.

```sh
docker compose --profile oidc up --build   # adds Keycloak on :8081 and gedour with OIDC on :8082
```

<http://localhost:8082> signs in through Keycloak's test realm (`alice`/`alice`, `bob`/`bob`,
`deploy/keycloak/realm.json`).

| Variable | Default | Purpose |
|---|---|---|
| `GEDOUR_KC_PORT` | `8081` | Keycloak port, the same inside and outside the container (it is in the issuer URL) |
| `GEDOUR_OIDC_PORT` | `8082` | Host port of the OIDC-mode gedour |
| `GEDOUR_HTTP_PORT` / `GEDOUR_METRICS_PORT` | `8080` / `9090` | Host ports of the dev-mode gedour |
| `GEDOUR_DB_PORT` | `55432` | Host port of PostgreSQL |
| `GEDOUR_MAILPIT_SMTP_PORT` / `GEDOUR_MAILPIT_PORT` | `1025` / `8025` | Host ports of Mailpit's SMTP and UI |

The realm's redirect and post-logout URIs pin the app ports: `localhost:8082` for compose, `localhost:18080`
for `make e2e`. Changing `GEDOUR_OIDC_PORT` (or `E2E_PORT`) needs the same change in `realm.json`.

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `GEDOUR_DATABASE_URL` | (required) | PostgreSQL URL, e.g. `postgres://user:pass@host:5432/gedour` |
| `GEDOUR_ROLE` / `--role` | `all` | `web`, `worker` or `all` |
| `GEDOUR_HTTP_ADDR` | `:8080` | UI, `/api`, `/healthz` |
| `GEDOUR_METRICS_ADDR` | `:9090` | `/metrics`, `/healthz`, `/livez` |
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
| `GEDOUR_WORKER_CONCURRENCY` | `8` | Runs (and deliveries) one worker process executes at once |
| `GEDOUR_TELEGRAM_API_URL` | `https://api.telegram.org` | Telegram Bot API base URL |
| `GEDOUR_SMTP_ADDR` | | `host:port` of the SMTP server; email channels are disabled without it |
| `GEDOUR_SMTP_FROM` | (required with SMTP) | Sender, e.g. `gedour <gedour@example.com>` |
| `GEDOUR_SMTP_USERNAME` / `GEDOUR_SMTP_PASSWORD` | | SMTP login (PLAIN), set both or neither |
| `GEDOUR_SMTP_TLS` | `starttls` | `starttls`, `tls` (implicit) or `none`; `none` only reaches loopback or `GEDOUR_SMTP_PLAIN_NETS`, and refuses a login |
| `GEDOUR_SMTP_PLAIN_NETS` | | Comma-separated CIDRs `GEDOUR_SMTP_TLS=none` may reach besides loopback (a sink on a container network) |

The web role needs either the three `OIDC_*` variables plus `GEDOUR_PUBLIC_URL`, or `GEDOUR_DEV_USER`;
both or neither refuse to start. The session cookie is `__Host-gedour_session` (`Secure`) with OIDC, plain
`gedour_session` in dev mode: serve gedour over HTTPS
(browsers make an exception for `http://localhost` only).

OIDC client settings at the provider (Keycloak: confidential client, standard flow, PKCE S256):

- Valid redirect URIs: `<GEDOUR_PUBLIC_URL>/auth/callback`
- Valid post logout redirect URIs: `<GEDOUR_PUBLIC_URL>/auth/signed-out` (without it, sign-out stops on
  the provider's error page)

Without a session the UI shows a sign-in gate and calls nothing but `/api/v1/me`; "Sign in" goes to the
provider and back to the page first asked for. A session that ends later (any 401, or the inbox stream
signing out) brings the gate back. Before sending the browser to the provider, `/auth/login` checks that
its authorization endpoint answers (3 s timeout, one check shared by concurrent logins; skipped when that
endpoint is on another host or port than the issuer); when it does not, or the callback's token or key fetch gets
no answer within 10 s, the gate shows "Sign-in service unavailable". A rejected callback (state, code, ID token,
session write) shows "Sign-in failed, try again". Dev mode signs in by itself and never renders the gate;
a failed dev sign-in answers a plain 500.

## Collection

- The worker role claims due watches from PostgreSQL (`FOR UPDATE SKIP LOCKED`, 5 min lease): replicas
  share the load, each due run executes once, and a crashed worker's watch runs again after the lease.
  Runs time out after 60 s (stored as an error, with backoff); on SIGTERM in-flight runs get
  `GEDOUR_SHUTDOWN_TIMEOUT` to finish. A run-now, resume or edit during a run never starts a second one:
  the watch reads `running: true`, and the request is kept for when the run ends.
- Default schedule: `1h` for GitHub watches (unauthenticated GitHub allows 60 requests an hour per
  address), `15m` for the others; any schedule can be set explicitly.
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

## Routing and channels

- Filters are [expr](https://expr-lang.org) expressions over `event` (`type`, `payload`, `occurred_at`) and
  `watch` (`id`, `name`, `kind`), plus `semver_gt(a, b)`; they must return a boolean. They compile on save:
  an error is a 400 on `body.filter` with its line and column. Empty matches every event. Only an
  allowlist compiles: literals (arrays of up to 50), `event`/`watch` fields, comparisons, `and or not`,
  `in`, `contains`, `startsWith`, `endsWith`, up to 3 `matches` outside predicates with a string-literal
  pattern of at most 64 regexp instructions and repetitions up to 32, `+`/`-` on numbers, times
  and durations, `?:`, constant ranges up to 100, `len lower upper trim trimPrefix trimSuffix int float
  abs now duration date string semver_gt`, and `any all none one filter count` with a predicate that
  holds no other predicate and reads only its item (`#`, `#.field`; compare `event`/`watch` fields outside
  it), in 200 expression nodes at most. On top, a static cost model prices every filter before it is saved
  (each operation 32 units, times 200 inside a predicate; string work a unit per byte, 3 for
  `lower`/`upper`, 12 for `semver_gt`; a field counts 4 KiB, `#` 64 KiB over all items) and refuses one
  above 1,000,000 units, about 5 ms on the worst payloads; the error gives the computed cost; `date()` takes no time zone argument; the second argument of `trim`, `trimPrefix` and `trimSuffix` is a
  string literal, for `trim` ASCII and at most 16 bytes. `GET /api/v1/filters/reference`
  serves this list and its limits, as the rule editor shows them. Filters see payload strings cut to 4 KiB (64 KiB in all) and arrays/maps to
  200 entries. Previews are limited to 2/s per user (burst 5, per replica). A user holds at most 100
  rules and 50 channels.
- The worker role routes each new event once (`FOR UPDATE SKIP LOCKED`): every enabled rule whose owner can
  read the watch now (own or shared) is evaluated; a match adds one inbox item per user and one delivery
  per channel. A filter failing on an event counts as no match (`gedour_rule_eval_errors_total`).
  Events stored before this version are not routed.
- Deliveries retry after 30 s, 2 min, 10 min and 30 min; the 5th failure is final (`dead`). A failed or
  dead delivery can be retried by hand. Lease and backoff use the database clock. A delivery whose
  channel owner can no longer read the watch is not sent (`dead`). On SIGTERM, sends in flight get
  `GEDOUR_SHUTDOWN_TIMEOUT`; past it they are cancelled and released, not counted as attempts.
- Webhooks: `POST` JSON `{delivery_id, event, watch, rule, sent_at}` with `X-Gedour-Signature:
  sha256=<hex HMAC-SHA256(secret, raw body)>`, `X-Gedour-Event`, `X-Gedour-Delivery`. 2xx is success;
  redirects are not followed; 10 s timeout. Same SSRF guard as collectors; a literal private IP is refused
  on save. A signing secret left empty is generated and returned once, in the create response.
- Telegram: `sendMessage` in HTML mode with the bot token as secret. Email: multipart text + HTML through
  the instance's SMTP server, at most 10 recipients per channel.
- Inbox live updates: `GET /api/v1/inbox/stream` (SSE). The router `NOTIFY`s `gedour_inbox`; each web
  replica holds one `LISTEN` connection. Streams send `event: inbox` with `{"unread": n}`, a comment every
  25 s, and `event: signed_out` when the session ends (heartbeats check the session without refreshing
  it: an open tab does not defeat the idle timeout). At most 10 streams per user and 2000 per replica.
- Metrics: `gedour_events_routed_total`, `gedour_rule_eval_errors_total`,
  `gedour_deliveries_total{channel_type,status}`, `gedour_delivery_duration_seconds{channel_type}`,
  `gedour_sse_subscribers`.

## Script channels

- A script (JavaScript, [goja](https://github.com/dop251/goja), 64 KiB at most, 50 per user) defines
  `function main(event, ctx)`. It sees `event` (`type`, `occurred_at`, `payload`, `summary`, `url`),
  `watch` (`id`, `name`, `kind`), `rule` (`id`, `name`), `delivery_id` (also on `ctx`), `log(...)` and
  `http.fetch(url, {method, headers, body})` → `{status, ok, headers, text}`. Nothing else: no `require`,
  timers, filesystem, process or environment. A throw (or a missing `main`) fails the delivery, which
  retries like any other. A delivery keeps only the logs; the return value (JSON, 16 KiB) shows in dry
  runs. Syntax is checked on save (400 on `body.source` with line and column).
- Each run is a child process: the binary re-executed as `gedour sandbox`, empty environment, cwd `/`,
  JSON lines over stdin/stdout, stderr discarded. The child caps its heap (GC target 128 MiB, exit past
  192 MiB), address space (startup size + 512 MiB), open files (16), file writes (none), CPU time
  2 × (timeout + 2 s) (GOMAXPROCS 2, all threads), kernel SIGKILL 1 s later, and each regexp match
  (the timeout). The parent interrupts it at 5 s (fetches
  included) and kills it 0.5 s later, and caps its output (2 MiB a message, 8 MiB a run). A child whose
  parent is gone, or still running 2 s past its timeout, exits by itself. Per process, deliveries run
  4 children at once (others wait); dry runs and channel tests 2 more, a 429 when both are busy, and
  their child dies when the client goes away.
- `http.fetch` is made by the parent with the webhook rules: SSRF guard, https (http for
  `GEDOUR_ALLOW_HTTP_HOSTS`), no redirect followed, 10 s, 10 calls per run, 256 KiB body, 1 MiB answer;
  `Host`, `Content-Length` and hop-by-hop headers are refused. Logs: 200 lines, 16 KiB, stored on the
  delivery (`logs`).
- Dry run: the same sandbox, but `http.fetch` only records the call (URL userinfo, and the values of
  secret-looking headers, query and fragment keys, redacted) and answers 200 with an empty body.
  1 per second per user (burst 3, per replica), not audited.
- A script channel's test (`POST /api/v1/channels/{id}/test`) is a live run on a test event: its
  fetches are sent, and the answer carries its `logs` and `error`. With both interactive sandboxes
  busy it is a 429 that neither writes an audit row nor starts the 10 s interval.
- Script changes are audited with the source's SHA-256 and size, not the source. A script cannot be
  deleted while a channel runs it (409 naming them).
- Metrics: `gedour_script_runs_total{outcome=ok|error|timeout|killed}`, `gedour_script_run_duration_seconds`.

## Kubernetes

`deploy/k8s` is a namespace-less Kustomize base: `kubectl apply -k deploy/k8s` deploys into the current
namespace. Overlays reference `deploy/k8s/base`.

| Object | Content |
|---|---|
| Deployment `gedour-web` | `--role=web`, 2 replicas, ports `http` 8080 and `metrics` 9090 |
| Deployment `gedour-worker` | `--role=worker`, 2 replicas, port `metrics` 9090 |
| Service `gedour` | web pods: `http` 8080, `metrics` 9090 |
| Service `gedour-worker` | worker pods: `metrics` 9090 |
| PodDisruptionBudgets `gedour-web`, `gedour-worker` | `minAvailable: 1`, `unhealthyPodEvictionPolicy: AlwaysAllow` |
| ConfigMap `gedour` (generated, hash suffix) | `GEDOUR_LOG_FORMAT=json`, `GEDOUR_LOG_LEVEL=info`, `GEDOUR_SHUTDOWN_TIMEOUT=15s` |

Both Deployments load the ConfigMap and a Secret `gedour` as environment (`envFrom`). The base ships no
Secret: create it first. Pods stay in `CreateContainerConfigError` until it exists.

| Secret key | Web | Worker | |
|---|---|---|---|
| `GEDOUR_DATABASE_URL` | required | required | |
| `GEDOUR_ENCRYPTION_KEY` | required | required | Same value on every replica |
| `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET` | required | unused | |
| `GEDOUR_SMTP_ADDR`, `GEDOUR_SMTP_FROM`, `GEDOUR_SMTP_USERNAME`, `GEDOUR_SMTP_PASSWORD` | optional | optional | Worker sends deliveries, web sends channel tests |

```sh
kubectl create secret generic gedour \
  --from-literal=GEDOUR_DATABASE_URL='postgres://gedour:...@db:5432/gedour' \
  --from-literal=GEDOUR_ENCRYPTION_KEY="$(openssl rand -base64 32)" \
  --from-literal=OIDC_ISSUER=https://sso.example.com/realms/example \
  --from-literal=OIDC_CLIENT_ID=gedour --from-literal=OIDC_CLIENT_SECRET='...'
```

Other settings (table in [Configuration](#configuration)) go into the ConfigMap from an overlay:
`GEDOUR_PUBLIC_URL` (web, required with OIDC), `GEDOUR_ALLOW_HTTP_HOSTS` and `GEDOUR_ALLOW_PRIVATE_NETS`
(both roles: web validates watch URLs, the worker fetches), `GEDOUR_WORKER_POLL` and
`GEDOUR_WORKER_CONCURRENCY` (worker), `GEDOUR_TELEGRAM_API_URL`, `GEDOUR_SMTP_TLS` and `GEDOUR_SMTP_PLAIN_NETS`
(both). Telegram bot tokens belong to channels, in the database. Never set `GEDOUR_DEV_USER` in a cluster.

```yaml
# kustomization.yaml of an overlay
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: gedour
resources:
  - https://github.com/gedour/gedour//deploy/k8s/base?ref=v0.1.0
components:
  - https://github.com/gedour/gedour//deploy/k8s/components/networkpolicy?ref=v0.1.0
images:
  - name: ghcr.io/gedour/gedour
    newTag: v0.1.0
configMapGenerator:
  - name: gedour
    behavior: merge
    literals:
      - GEDOUR_PUBLIC_URL=https://gedour.example.com
```

- Image: `ghcr.io/gedour/gedour`. Releases push `vX.Y.Z`, `X.Y`, `X` and `latest` (pre-releases: the
  tag itself plus `alpha`, `beta` or `rc`); every push to `develop` pushes `develop-<sha>-<run>`, the run
  number ordering them for image automation. The base pins `v0.1.0` through `images:`. Pin a full
  version (or a digest) in overlays, never a moving tag. A release tag fails before the build unless
  the base `newTag` and the README's `?ref=` and `newTag:` pins equal it (`scripts/check-release-pin.sh`; pre-releases skip it).
- Probes: startup and readiness on `/healthz` (web on `http`, worker on `metrics`: it has no main
  listener), liveness on `metrics` `/livez`. At startup the process retries PostgreSQL with backoff
  (0.5 s doubling to 10 s), then applies migrations and opens its listeners; the startup probe allows
  5 minutes for that, so a pod started during a database outage becomes ready once PostgreSQL is back.
  Later, `/healthz` fails while PostgreSQL is unreachable and readiness drops after 20 s; `/livez` does
  not look at PostgreSQL, so an outage never restarts the pods.
- Shutdown: `terminationGracePeriodSeconds: 30` covers `GEDOUR_SHUTDOWN_TIMEOUT` (15 s) and, on web, a
  5 s `preStop` sleep that lets the ingress drop the endpoint first. The `sleep` action needs
  Kubernetes 1.30 or later (the image has no shell). Raising the timeout means raising the grace period.
- Scaling: any number of replicas of either role. Migrations run at startup under a PostgreSQL advisory
  lock (other replicas wait), workers claim runs with `FOR UPDATE SKIP LOCKED`, sessions and the inbox
  fan-out live in PostgreSQL, so no sticky sessions. Dry-run and preview rate limits are per replica.
  Pods spread across nodes when they can (`topologySpreadConstraints`, `ScheduleAnyway`).
- Security: UID/GID 65532, `runAsNonRoot`, read-only root filesystem, no capabilities, no privilege
  escalation, `RuntimeDefault` seccomp, no service account token. The script sandbox runs under all of it.
- Memory: sandbox children live in the pod and count against its limit. A child dies past 192 MiB of
  heap (about 210 MiB resident). Worker: up to 4 children plus the parent (about 20 MiB busy),
  request 320 MiB, limit 1 GiB. Web: up to 2 interactive children, request 256 MiB, limit 640 MiB. `GEDOUR_WORKER_CONCURRENCY` does not add
  children: deliveries run 4 scripts per process at most. No CPU limit; a child caps its own CPU time.

### Ingress

Not in the base. `/api/v1/inbox/stream` is a server-sent events stream that stays open and sends a comment
every 25 s, with `X-Accel-Buffering: no`: the proxy's read/idle timeout must exceed 25 s (60 s or more)
and it must not buffer responses on that path.

```yaml
# apiVersion: networking.k8s.io/v1
# kind: Ingress
# metadata:
#   name: gedour
#   annotations:
#     # ingress-nginx; other controllers have their own timeout settings.
#     nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
#     nginx.ingress.kubernetes.io/proxy-buffering: "off"
# spec:
#   ingressClassName: nginx
#   tls:
#     - hosts: [gedour.example.com]
#       secretName: gedour-tls
#   rules:
#     - host: gedour.example.com
#       http:
#         paths:
#           - path: /
#             pathType: Prefix
#             backend:
#               service:
#                 name: gedour
#                 port:
#                   name: http
```

### Network policy

`deploy/k8s/components/networkpolicy` is an optional component. Ingress: port 8080 from anywhere (the
ingress controller usually runs in another namespace), 9090 from the same namespace. Egress to any
address: DNS, 5432, 443, and SMTP on 587, 465 and 25. Add rules in an overlay when Prometheus scrapes
from another namespace, or a webhook, script fetch or watch target listens elsewhere than 443.

### Smoke test

`make k8s-smoke` builds the image, creates a kind cluster (`gedour-smoke`), deploys `deploy/k8s/test` (the
base plus a throwaway PostgreSQL, the fake upstream, dev mode and the network policy; test only), and
checks: gedour waits for PostgreSQL, started only afterwards, then 2 web and 2 worker pods are ready
without restarts, `/healthz` and `/livez` answer through both Services, a script dry run
and a script over its memory cap (killed, pod alive), a watch run and script deliveries on the workers.
Then it brings up `compose.yaml` under a fresh project and checks the dev-mode UI, `/api/v1/me` and
`/metrics`. Cluster, compose project and the images the smoke built are removed at the end. Needs kind
v0.23.0 or later (its node runs Kubernetes 1.30+, for the `preStop` sleep action; found in `PATH` or
`KIND=<path>`, tested with v0.33.0), kubectl, Docker, Go, curl and jq. Ports:
`K8S_SMOKE_PORT`, `K8S_SMOKE_FAKE_PORT`, `COMPOSE_SMOKE_PORT`, `COMPOSE_SMOKE_METRICS_PORT`,
`COMPOSE_SMOKE_DB_PORT`.

## Endpoints

- `GET /healthz`: 200 when PostgreSQL answers, 503 otherwise.
- `GET /livez` (metrics port only): 200 while the process serves, whatever PostgreSQL does.
- `GET /api/openapi.json`: OpenAPI document.
- `GET /api/v1/version`: build information.
- `GET /auth/login?return_to=/path`, `GET /auth/callback`: sign-in flow. On failure both answer 303 to
  `return_to` (`/` when unknown) with `auth=unavailable` (provider or token endpoint unreachable) or
  `auth=failed` added; the rest of `return_to`, fragment included, is kept as sent.
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

- `/api/v1/channels` (`GET`, `POST`), `/api/v1/channels/{id}` (`GET`, `PATCH`, `DELETE`, 409 while a rule
  uses it), `POST /api/v1/channels/{id}/test` (one test per 10 s per user, 429 otherwise), `GET /api/v1/channel-types`.
  Secrets are write-only (`last4` only).
- `/api/v1/rules` (`GET`, `POST`), `/api/v1/rules/{id}` (`GET`, `PATCH`, `DELETE`),
  `POST /api/v1/rules/preview {watch_id, filter}`: the filter on the last 20 readable events in scope.
  `GET /api/v1/filters/reference`: what a filter may use, and its limits.
- `GET /api/v1/deliveries?channel_id=&status=&cursor=`, `POST /api/v1/deliveries/{id}/retry`.
- `/api/v1/scripts` (`GET` without sources, `POST`), `/api/v1/scripts/{id}` (`GET`, `PATCH`, `DELETE`),
  `POST /api/v1/scripts/{id}/dry-run` and `POST /api/v1/scripts/dry-run {source}`, each with
  `{event_id}` (a readable event) or `{sample: {type, payload, occurred_at, watch}}`: answers
  `{ok, outcome, error, logs, result, fetches, duration_ms}`.
- `GET /api/v1/inbox?state=unread|all|archived&cursor=`, `GET /api/v1/inbox/count`,
  `POST /api/v1/inbox/read|unread|archive|unarchive {ids}` (the caller's items only), `GET /api/v1/inbox/stream`.
- Channels, rules, scripts and deliveries are private to their owner (404 for others); their changes, channel
  tests and delivery retries are audited. Inbox items and deliveries of a watch the user can no longer
  read are hidden, and a rule's `watch_name` is null then.

The full contract is `api/openapi.json` (also served at `/api/openapi.json`).

Every other `/api` route needs a session (401 otherwise).

## Development

See `AGENTS.md` for build, test, lint and e2e commands.

## License

Apache License 2.0, see [`LICENSE`](./LICENSE) and [`NOTICE`](./NOTICE).

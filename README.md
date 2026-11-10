<p align="center"><img src=".github/banner.svg" alt="gedour: All the upstream. None of the noise." width="100%"></p>

# gedour

Self-hosted watch service: follows upstream releases, issues, discussions, pull/merge requests and image tags, keeps their
last state, and sends only the changes that matter to the channels you pick.

The name is Breton: *gedour* (pronounced like *get* + *door*, hard G, /ˈɡeː.dur/) means lookout,
watchman. It comes from *ged*, "watch", plus the agent suffix *-our*, and is attested as *guedour* in 1499
([Wiktionnaire](https://fr.wiktionary.org/wiki/gedour), [Devri](https://devri.bzh/dictionnaire/g/gedour/)).

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

<http://localhost:8082> signs in through Keycloak's test realm (`deploy/keycloak/realm.json`): `alice`/`alice`
(admin), `bob`/`bob` (user), `carol`/`carol` (no role, refused), `dave`/`dave` (user; the e2e suite
purges it).

| Variable | Default | Purpose |
|---|---|---|
| `GEDOUR_KC_PORT` | `8081` | Keycloak port, the same inside and outside the container (it is in the issuer URL) |
| `GEDOUR_OIDC_PORT` | `8082` | Host port of the OIDC-mode gedour |
| `GEDOUR_HTTP_PORT` / `GEDOUR_METRICS_PORT` | `8080` / `9090` | Host ports of the dev-mode gedour |
| `GEDOUR_DB_PORT` | `55432` | Host port of PostgreSQL |
| `GEDOUR_MAILPIT_SMTP_PORT` / `GEDOUR_MAILPIT_PORT` | `1025` / `8025` | Host ports of Mailpit's SMTP and UI |

The realm's redirect and post-logout URIs pin `localhost:8082` for compose: changing `GEDOUR_OIDC_PORT` needs the
same change in `realm.json`. The `make e2e` entry follows `E2E_PORT`.

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `GEDOUR_DATABASE_URL` | (required) | PostgreSQL URL, e.g. `postgres://user:pass@host:5432/gedour` |
| `GEDOUR_ROLE` / `--role` | `all` | `web`, `worker` or `all` |
| `GEDOUR_HTTP_ADDR` | `:8080` | UI, `/api`, `/mcp`, `/healthz` |
| `GEDOUR_METRICS_ADDR` | `:9090` | `/metrics`, `/healthz`, `/livez` |
| `GEDOUR_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `GEDOUR_LOG_FORMAT` | `auto` | `auto` (console on a TTY, else JSON), `console`, `json` |
| `GEDOUR_SHUTDOWN_TIMEOUT` | `15s` | Drain time on SIGTERM |
| `OIDC_ISSUER` | | OIDC provider issuer URL |
| `OIDC_CLIENT_ID` | | OIDC client ID (confidential client, authorization code + PKCE) |
| `OIDC_CLIENT_SECRET` | | OIDC client secret; also keys the short-lived login state cookie |
| `GEDOUR_OIDC_ROLES_CLAIM` | `roles` | ID token claim holding the user's roles: a top-level key (`https://example.com/roles` works), else a dotted path (`realm_access.roles`) |
| `GEDOUR_OIDC_REQUIRED_ROLE` | | Role needed to sign in; empty lets any authenticated user in |
| `GEDOUR_OIDC_ADMIN_ROLE` | | Role that makes an admin, and also grants sign-in; empty means no admin |
| `GEDOUR_PUBLIC_URL` | (required with OIDC) | Comma-separated browser-facing base URLs, one per host (e.g. `https://gedour.example.com,https://gedour.internal.example.com`). A request uses the entry whose `host:port` matches its `Host` header, else the first; the redirect URI is `<url>/auth/callback` |
| `GEDOUR_DEV_USER` | | Dev single-user mode: every `/api` and `/auth` request runs as this user, and `/mcp` still takes a key. All three answer only on a loopback Host (`localhost`, `127.0.0.1`, `::1`), others get 403. Never in production |
| `GEDOUR_SESSION_TTL` | `168h` | Absolute session lifetime |
| `GEDOUR_SESSION_IDLE_TIMEOUT` | `24h` | Session ends after this long without a request |
| `GEDOUR_API_KEY_SIGNIN_MAX_AGE` | `720h` | An API key goes dormant when its owner has not signed in for this long; a sign-in wakes it. Keep it above `GEDOUR_SESSION_TTL`, or a user with a live session sees a dormant key with no sign-in to do. No effect in dev mode, but must still be a positive duration |
| `GEDOUR_ENCRYPTION_KEY` | (required) | Base64 of 32 random bytes (`openssl rand -base64 32`); AES-256-GCM key of stored credentials. Losing it makes them unreadable |
| `GEDOUR_ALLOW_HTTP_HOSTS` | | Comma-separated hosts a watch URL (`api_url`, `base_url`, an OCI `registry`), a webhook, a Nextcloud Talk URL or a script fetch may reach over plain http; others need https |
| `GEDOUR_ALLOW_PRIVATE_NETS` | | Comma-separated CIDRs outbound requests may reach despite the SSRF guard, e.g. `10.20.0.0/16` for an internal registry or a Nextcloud on a private network; also opens webhook targets |
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
- With several `GEDOUR_PUBLIC_URL` entries, register both URIs for each of them. The reverse proxy must
  pass the browser's `Host` header through, or every request gets the first entry.

Without a session the UI shows a sign-in gate and calls nothing but `/api/v1/me`; "Sign in" goes to the
provider and back to the page first asked for. A session that ends later (any 401, or the inbox stream
signing out) brings the gate back. Before sending the browser to the provider, `/auth/login` checks that
its authorization endpoint answers (3 s timeout, one check shared by concurrent logins; skipped when that
endpoint is on another host or port than the issuer); when it does not, or the callback's token or key fetch gets
no answer within 10 s, the gate shows "Sign-in service unavailable". A rejected callback (state, code, ID token,
session write) shows "Sign-in failed, try again". Dev mode signs in by itself and never renders the gate;
a failed dev sign-in answers a plain 500.

### Restricting access with roles

- gedour reads the roles from the ID token's `GEDOUR_OIDC_ROLES_CLAIM`, a string array. A missing claim,
  another type or non-string items count as no role.
- With `GEDOUR_OIDC_REQUIRED_ROLE` set, a user holding neither it nor `GEDOUR_OIDC_ADMIN_ROLE` lands on the
  gate with "No access to gedour": no session, no account created, an audit entry `user.login_denied`.
- Check the role name before enabling `GEDOUR_OIDC_REQUIRED_ROLE`: a sign-in refused for a wrong name suspends that user's API keys until a sign-in passes the check.
- Roles are read at sign-in only: a role added or removed at the provider applies at the next sign-in.
  A sign-in refused for a missing role also ends the account's sessions and suspends its active API keys (status
  `suspended`, each call gets 401; the audit entry holds `deleted_session_count` and `suspended_api_key_count`, when
  above 0), even if the browser drops the callback. The next successful sign-in lifts the suspension (`user.login` holds
  `resumed_api_key_count`); a key that expired or was revoked meanwhile stays dead.
  Sessions opened before the upgrade that added roles are not admin until their next sign-in.
- `GET /api/v1/me` reports `is_admin`. Dev mode's user is always admin.

Microsoft Entra ID:

1. App registration → App roles: create e.g. `gedour-user` and `gedour-admin` (allowed member types: Users/Groups).
2. Enterprise application → Users and groups: assign users or groups to those roles.
3. The assigned app roles arrive in the ID token's `roles` claim: keep the default `GEDOUR_OIDC_ROLES_CLAIM`.

Keycloak:

1. Client → Roles: create `gedour-user` and `gedour-admin`; assign them to users or groups.
2. Client → Client scopes → the dedicated scope → Add mapper → By configuration → "User Client Role": client
   ID your client, token claim name `roles`, multivalued, "Add to ID token" on.
3. Without that mapper, the built-in `resource_access.<client>.roles` claim is in the access token only:
   turn "Add to ID token" on in the "client roles" mapper of the `roles` client scope, then set
   `GEDOUR_OIDC_ROLES_CLAIM=resource_access.<client>.roles`.

The compose realm (`deploy/keycloak/realm.json`) is set up this way.

### Administration

- Admins (`GEDOUR_OIDC_ADMIN_ROLE` at sign-in, or the dev user) get an Administration entry in the
  navigation: every account with its last sign-in (never-signed-in first, then oldest), status (active,
  disabled, purged) and what it owns (watches, channels, rules, scripts, credentials).
- Disable (not your own account): every session of the account ends at once (its next call gets 401),
  its API keys are revoked for good (enabling does not bring them back),
  its sign-in is refused with "No access to gedour" (`user.login_denied`, reason `disabled`), its rules
  route nothing and it gets no inbox items. Deliveries already queued for its channels end unsent
  ("not sent: the channel's owner is disabled"). Its watches stop collecting until it is enabled again. Its
  data stays.
- Enable restores sign-in, routing for new events and collection. A due time that passed while disabled
  moves to now; a future one (a rate-limit hold) stays, and so does a leased watch's. The next run reports what changed
  since the watch's last run, as far as the forge still lists it. Inbox items skipped while disabled are not
  replayed; the owner can retry dead deliveries by hand.
- Both are audited (`user.disable`, `user.enable`) with the status before and after; a disable also holds
  `revoked_api_key_count` when it revoked any.
- Transfer (an account's watch count opens its watches): the watch goes to another active account. Its
  credential goes with it, renamed `<name> (transferred)` (then `(transferred 2)`, ...) on a name clash;
  the secret still decrypts. A credential another watch of the old owner uses refuses the transfer (409,
  those watches listed). A due time gone stale moves to now. The old
  owner loses access; its rules targeting that watch stay and route nothing while it does not own the watch
  (a transfer back restores routing).
  Audited as `watch.transfer` (owners, credential; never the secret).
- Purge (disabled accounts only): one transaction deletes the account's watches, channels, rules,
  scripts, credentials, API keys and inbox, and blanks its email and name. It also deletes the rules other
  users still hold on its watches after a transfer; the confirmation lists them. The account row stays
  as "purged", with its provider subject, so the same person stays refused at sign-in. The audit log is
  append-only and untouched: entries written before the purge (such as the account's creation) keep
  what they recorded. Audited as `user.purge` with the deleted counts, never the email or name.

#### Acting as a user

- "Act as" on an account row (not your own, not a purged one; a disabled one is fine, to fix it before
  enabling it): your session runs as that user. Every page shows their data, inbox included, under a
  banner with "Stop". Stop returns the session to your own data. Signing out ends your session.
- While acting, the Administration pages are hidden and every admin API call answers 409, except
  `DELETE /api/v1/admin/act-as` (stop).
- Audit: every change made while acting records you as actor and the user as `on_behalf_of`. The user
  sees those entries in their audit view as "<you> for <them>", plus the start and the stop
  (`session.act_as_start`, `session.act_as_stop`); you see them in yours.
- Purging an account sends every admin session acting as it back to its admin (`acting_sessions` in the
  preview and the audit).

## API keys

The API keys page creates a key for a program acting as you: a name, a scope and a duration. The key starts
with `gdr_` and shows once, with a copy button. gedour stores only its SHA-256. At most 10 keys can be active;
keys dead for over 30 days go away at the next creation. A session acting as a user cannot create one.

- **Duration**: 7, 30, 90 (default) or 365 days, or no end date. To rotate, create a new key, then revoke the old one.
- **Dormancy**: a key, even one with no end date, stops working once you have not signed in for
  `GEDOUR_API_KEY_SIGNIN_MAX_AGE` (30 days by default). The next sign-in wakes it. The page shows each key's
  effective end, the earlier of its expiry and that limit. Dev mode has no such limit.
- **Revocation**: revoking is final. Disabling your account revokes every active key too; purging an account deletes them.
- **Suspension**: a sign-in refused for a missing role suspends every active key (401 on each call) until your next
  successful sign-in, which restores the ones neither revoked nor expired meanwhile.

| Scope | Reaches | Does not reach |
|---|---|---|
| `read` (default) | Your inbox with release notes, your watches with their config and the attached credential's ID, rules with filters, deliveries with their channel's name and without logs or errors | Credential secrets, channel targets, scripts, audit, other users' data, admin, other keys |
| `write` | Also inbox state, and scheduling a run of one of your watches | Creating or changing anything else |
| `manage` | Also the list of channels (ID, name, type), creating and editing rules and watches, within [the refusals below](#mcp) | Credential secrets, channel targets, delete |

A hijacked `manage` key can silence alerts by disabling a rule or pausing a watch, route events to your existing
channels, and put a watch name of its choosing, and text and links from a repository it controls, in their
messages: give it only to clients that confirm writes with a person. It opens no new destination, with one limit: a
known host that forwards a request's path on a redirect, or names a token host in a registry challenge, carries the
key's `repo`, `project` or `repository` to wherever that host says, with no credential. Keys never open the REST API, which stays session-only.

Creating and revoking are audited (`api_key.created`, `api_key.revoked`) with the key's ID, name, scope and
expiry, never the key or its hash.

## MCP

`/mcp` is an [MCP](https://modelcontextprotocol.io) server over Streamable HTTP, stateless and JSON only, for
protocol versions up to `2026-07-28`. A client authenticates with an API key on every request. Serve it over
HTTPS: the key is a bearer credential.

```sh
claude mcp add --scope user --transport http gedour https://gedour.example.com/mcp --header "Authorization: Bearer <key>"
```

Or per project, in `.mcp.json`, with the key in an environment variable:

```json
{
  "mcpServers": {
    "gedour": {
      "type": "http",
      "url": "https://gedour.example.com/mcp",
      "headers": { "Authorization": "Bearer ${GEDOUR_API_KEY}" }
    }
  }
}
```

| Tool | Returns | Limit |
|---|---|---|
| `count_inbox` | Unread inbox items | |
| `list_inbox` | Inbox items with their event; a body over 8 KiB is cut and says so | default 20, max 50 |
| `list_watches` | Your watches, with config; `truncated: true` when it returns as many as the limit | default 50, max 200 |
| `get_watch` | One watch | |
| `list_watch_events` | A watch's events | max 50 |
| `list_watch_states` | A watch's observed states | max 50 |
| `list_rules` | Routing rules, with filters | |
| `get_rule` | One rule | |
| `list_deliveries` | Deliveries with their channel's name, without logs or errors, as errors name channel targets | max 50 |
| `mark_inbox_read`, `mark_inbox_unread` | Mark items read or unread (unread also unarchives): `updated` and `unread`; `ids` is an array of item IDs from `list_inbox`, another user's IDs are ignored | `write` key; max 500 IDs |
| `archive_inbox`, `unarchive_inbox` | Archive items or take them back: `updated` and `unread`; `ids` is an array of item IDs from `list_inbox`, another user's IDs are ignored | `write` key; max 500 IDs |
| `run_watch` | Schedules a run of one of your watches and does not wait; refused on a paused watch, or while the upstream's rate limit holds the watch | `write` key; shared budget |
| `get_filter_reference` | What a rule's filter may use, and its limits | `manage` key |
| `list_channels` | Your channels: ID, name and type, nothing else | `manage` key |
| `create_watch` | Creates a watch without a credential | `manage` key; shared budget; refused past 200 owned watches |
| `update_watch` | Changes a watch's name, config, schedule or paused state | `manage` key; shared budget |
| `create_rule`, `update_rule` | Create or change a routing rule, with the same checks as the UI | `manage` key; shared budget; at most 100 rules |

A key sees what its owner sees through the REST API. A `read` key reaches the first nine rows and gets `-32602` on the
others. A `write` key also changes inbox state and schedules watch runs, nothing else. A `manage` key reaches every row.
Only `create_watch` and `update_watch` take a free-form object, `config`. No tool takes a credential, and no input names a URL or a host
outside that `config`. The shared budget is a burst of 6, then one every 10 s, per key and replica, across `run_watch` and the four edits.

A `manage` key has three refusals, because a config decides where gedour sends requests and with which token:

- **No credential**: `create_watch` and `update_watch` refuse `credential_id`, even one you hold, and cannot detach one. A private repository starts in the UI.
- **No retarget of a watch with a credential**: its config cannot change through a key, yours included, or an injected prompt could send your token to another host. Name, schedule and paused still work.
- **Known hosts only**: `api_url`, `base_url` and `registry` must name, with no path, the kind's default (GitHub kinds `api.github.com`, GitLab kinds `gitlab.com`, `oci_tags` `docker.io` or `ghcr.io`), or the host and path of a watch you own. A default on another kind is refused, even if a watch of yours saved it. A new host or path starts in the UI, so a URL cannot carry data to an attacker's server, nor through a known host's redirect. Targets compare exactly, after folding ASCII host case, a trailing dot, a default port and a trailing `/`. `?` and `#` are refused for everyone. A non-ASCII host compares as it was saved: its punycode form is another host, and a homoglyph of a known host is refused.

While the upstream's rate limit holds a watch, a key's `update_watch` leaves its next run where it is: a new schedule, an unpause or a new config take effect, the run waits. A session's edit still runs it at once.

A host a watch targets may still answer a redirect to a host you never named. The worker follows up to 5, over HTTPS or to an allowed `http` host, through the same address checks, and drops `Authorization`, `Private-Token` and `Referer` when leaving the original host. A registry's token challenge names its token host the same way, with no credential on a key's watch.

`run_watch` writes `watch.run_requested`, and the manage edits their usual `watch.*` and `rule.*` rows, in the audit log with the key's ID, and the Audit page shows `via key <name>` to the key's owner, and the ID to others
or once the key is gone. Inbox marks are not audited, as on REST; the log lines cover them.

- **Untrusted output**: release notes, titles and errors come from third parties. Every successful result is one text block: any notice about the key, a warning, then the data
  fenced with a nonce. The data drops C0 and C1 controls but tab, LF and CR, format characters (Unicode Cf: bidi,
  zero-width, tags) and U+2028, U+2029. Other invisible characters, such as variation selectors, stay. No fence stops a model that decides
  to obey: review what your client does with the tools it holds.
- **Limits**: 5 requests/s per key with a burst of 20 (429 and `Retry-After`), per replica. On top, the shared budget above answers `isError`. A body over 1 MiB gets 413, a result over 512 KiB asks for a smaller `limit`. JSON-RPC batches are refused.
- **Methods**: at every protocol version `tools/list`, `tools/call` and `notifications/cancelled`; from `2026-07-28` `server/discover`; below it (a request with no version header counts) `initialize` and `ping`.
  `notifications/initialized` and `notifications/roots/list_changed` pass at every version, with no effect from `2026-07-28`. Any other method gets `-32601`, at 404 from `2026-07-28` and at 200 below it. An unserved notification gets a plain 404.
- **Refusals**: a missing or invalid key gets 401, an `Origin` outside `GEDOUR_PUBLIC_URL` 403; in dev mode, only
  loopback `Host` and `Origin` pass. A dormant key still lists tools, and each call answers with the URL to sign in at.
  In a key's last 7 days, each successful result starts with a notice saying when it stops. OAuth is not offered:
  `/.well-known/` and `/mcp/` answer 404.
- **Observability**: one `mcp_request` log line per request that reaches the SDK, written before it does (key ID, user ID, request ID, and the
  JSON-RPC method when the body has a served one), and one `mcp_tool_call` line per call (key ID, user ID, tool, outcome, duration, request ID, the one an internal error quotes);
  neither holds the key or the arguments. A refused method logs `mcp_method_refused`, without the method. In JSON format, the logger samples at 100 lines per second per message, then one in 100, but never `mcp_request`, `mcp_tool_call`, `mcp_handler_panic`, `mcp_dispatch_failed` or `mcp_dispatch_unexpected_status`. Metrics: `gedour_mcp_tool_calls_total{tool,outcome}` and `gedour_mcp_auth_failures_total`.

## Collection

- The worker role claims due watches from PostgreSQL (`FOR UPDATE SKIP LOCKED`, 5 min lease): replicas
  share the load, each due run executes once, and a crashed worker's watch runs again after the lease.
  Watches of a disabled owner are not claimed, and `gedour_watches_due` leaves them out.
  Runs time out after 60 s (stored as an error, with backoff); on SIGTERM in-flight runs get
  `GEDOUR_SHUTDOWN_TIMEOUT` to finish. A run-now, resume or edit during a run never starts a second one:
  the watch reads `running: true`, and the request is kept for when the run ends, unless that run ends on the upstream's rate limit, which sets the next run; a changed target still runs at once.
- Default schedule: `1h` for GitHub watches (unauthenticated GitHub allows 60 requests an hour per
  address), `15m` for the others; any schedule can be set explicitly.
- Next run: interval schedules ±10 % jitter, cron schedules at their next activation (UTC). Errors back
  off `interval × 2^(failures-1)`, capped at 8 intervals. Rate limits (GitHub `X-RateLimit-*`, GitLab
  `RateLimit-Reset`, `Retry-After`) push the next run to the reset time, at most 1 h away, without
  counting as a failure. Release watches send `If-None-Match`/`If-Modified-Since`; a 304 means unchanged. A GitHub
  repository watch with a new-items box on reads all its lists in full: a 304 on one would skip the others.
- The first run stores a baseline and emits nothing. Each later change becomes typed events
  (`release.published`, `release.promoted`, `issue.opened`, `pr.opened`, `discussion.opened`, `pr.state_changed`, `pr.draft_changed`,
  `pr.review_changed`, `pr.ci_changed`, `pr.commented`, `issue.state_changed`, `issue.labels_changed`,
  `issue.commented`, `discussion.state_changed`, `discussion.answer_changed`, `discussion.labels_changed`,
  `discussion.commented`, `issue.reacted`, `pr.reacted`, `discussion.reacted`, `mr.state_changed`, `mr.draft_changed`, `mr.approval_changed`,
  `mr.pipeline_changed`, `tag.created`, `tag.removed`). Changing a watch's target clears its states: the next
  run is a new baseline. The last 50 states per watch are kept; events are kept.
- `pr.commented` covers conversation comments, inline review comments and review bodies, one event per
  comment. A PR run makes 6 GitHub requests (7 past 100 conversation comments).
- A GitHub repository watch reports releases. Its boxes `new_issues` and `new_prs` (config, default `false`) add
  `issue.opened` and `pr.opened`, one per item opened after the box is checked; each run then makes 1 more
  request (the 50 newest issues and PRs, one list for both boxes). A run reports at most those 50: more than
  that opened between two runs are not reported. Payload: `number`, `title`, `author`,
  `labels` (names, `[]` without any), `url`, `created_at`. A box is not part of the target: toggling one keeps
  the release states, but it is a config change, so on a watch with a credential only its owner may toggle it,
  from the UI. The run stores the numbers listed, one list per checked box, not the items: the first run after a
  box is checked takes its kind as a baseline (an uncheck then a check with no run between keep the list going),
  and an item numbered at or below the oldest one stored for its kind is not new. Checking a box makes the watch
  due at once; items opened before that first run succeeds are in its baseline and are not reported. A paused
  watch takes the baseline when it resumes. To follow an item once opened, add a `github_pr`, `github_issue` or `github_discussion` watch.
- The box `new_discussions` (config, default `false`) adds `discussion.opened`, one per discussion opened after the
  box is checked, with the same baseline and due-now rules (list `discussions` in the state); only a number above every stored one is new. An issue converted to a discussion keeps its number: it is skipped when that number is at or below the highest stored, and reported otherwise (a recent issue, or any conversion while the stored list is empty). Each run then
  makes 2 to 3 more requests (1 with at most one discussion): a `per_page=1` probe whose `Link` last page number
  is the total, then the last 100-item page of the list (oldest first) and the page before it when the last holds
  fewer than 50. Only those pages are the window; a `Link` without a readable last page number fails the run. Payload: `number`, `title`,
  `author`, `category`, `labels` (names), `url`, `created_at`. A repository with discussions disabled (HTTP 410)
  fails the run with an error naming the box. These REST endpoints are undocumented by GitHub.
- A `github_issue` watch (config `repo`, `number`, `api_url`, like `github_pr`) follows one issue: 2 GitHub
  requests per run (3 past 100 comments). Events, each with `number`, `title`, `url`: `issue.state_changed`
  (`from`, `to`: `open` or `closed`, plus `state_reason` as GitHub reports it, e.g. `completed`, `not_planned`,
  `reopened`, `""` when none; a closed issue whose reason changes emits it too, `from` and `to` both `closed`), `issue.labels_changed` (`from`, `to`: label names, sorted) and `issue.commented`
  (`author`, `excerpt`; `url` is the comment's). A number that is a pull request fails the run: watch it with
  `github_pr`.
- A `github_discussion` watch (config `repo`, `number`, `api_url`, like `github_issue`) follows one discussion:
  2 GitHub requests per run (3 past 100 comments), on the same undocumented REST endpoints. Events, each with
  `number`, `title`, `url`: `discussion.state_changed` (`from`, `to`, `state_reason` as GitHub reports it, e.g.
  `resolved`, `outdated`, `duplicate`, `reopened`, `""` when none; a reason change while closed emits it too),
  `discussion.answer_changed` (`from`, `to`: answer comment URLs, `""` when none, plus `chosen_by`, `""` when
  removed), `discussion.labels_changed` (`from`, `to`: label names, sorted) and `discussion.commented` (`author`,
  `excerpt`, and `reply_to`, the parent comment's URL, on a reply; `url` is the comment's). The category is
  stored and shown; its change emits nothing. Issues, PRs and discussions are numbered in one sequence: a number that is
  not a discussion fails the run with an error pointing to `github_issue` and `github_pr`.
- The box `reactions` (config, default `false`) of `github_issue`, `github_pr` and `github_discussion` follows the
  counts of the 8 reaction kinds (`+1`, `-1`, `laugh`, `hooray`, `confused`, `heart`, `rocket`, `eyes`) on the
  initial post and on each of the 20 newest comments stored; review bodies, and an entry standing for several
  inline comments of one review, are not tracked. `github_issue` and `github_discussion` make no extra request;
  `github_pr` makes 1 more per run (`/issues/{n}`, for the PR body's counts). Event `<kind>.reacted`
  (`issue.reacted`, `pr.reacted`, `discussion.reacted`), one per target whose counts rose since the last run, every
  kind that rose in one event. Payload: `number`, `title`, `url` (the item's or the comment's), `target` (`item` or
  `comment`), `added` (kind to its rise, rises only) and `counts` (the 8 current counts). A drop emits nothing. The
  first run after the box is checked, and a comment new in the run, are a baseline. Toggling the box is no
  retarget, and checking it does not make the watch due. Who reacted is not reported.
- SSRF guard: outbound requests refuse loopback, private (RFC 1918, ULA), link-local (cloud metadata
  included), CGNAT, multicast and reserved addresses (IPv4 inside NAT64 and 6to4 included, Teredo refused),
  checked after DNS resolution on every connection,
  redirects included; a redirect to another host drops `Authorization`, `PRIVATE-TOKEN` and `Referer`. Environment
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
  `lower`/`upper`, 12 for `semver_gt`; a field counts 4 KiB, `event.payload.body` 64 KiB, `#` 64 KiB over all
  items) and refuses one above 4,000,000 units, about 45 ms on the worst payloads (about 4.5 s for a user's 100 rules), so a pattern on
  `body` holds 61 instructions at most; the error gives the computed cost; `date()` takes no time zone argument; the second argument of `trim`, `trimPrefix` and `trimSuffix` is a
  string literal, for `trim` ASCII and at most 16 bytes. `GET /api/v1/filters/reference`
  serves this list and its limits, as the rule editor shows them. Filters see payload strings cut to 4 KiB (64 KiB in all), `event.payload.body`
  to 64 KiB on top (128 KiB in all), and arrays/maps to 200 entries. A predicate on a string fails at evaluation.
  Release events of GitHub and GitLab carry the notes in `payload.body` (cut to 64 KiB) and the CVE ids of the
  whole notes in `payload.cves` (uppercased, deduplicated, 200 at most, each 32 bytes at most; `[]` without any). For example:
  `len(event.payload.cves) > 0`, `"CVE-2026-1234" in event.payload.cves`,
  `event.payload.body matches "(?i)breaking.change"`. On an event without them, filters read `cves` as `[]` and
  `body` as `""`; the stored and delivered payload stays as it is. So `len(event.payload.cves) == 0` and
  `event.payload.body == ""` also match non-release events: a rule meant for releases adds a check on
  `event.type` or targets a watch. New issues, PRs and discussions carry their label names in `payload.labels`
  (discussions add `payload.category`):
  `"bug" in event.payload.labels`, `event.type == "pr.opened" && event.payload.author != "dependabot[bot]"`. Reaction
  events carry their rises in `payload.added`: `event.type == "issue.reacted" && "heart" in event.payload.added`,
  `"+1" in event.payload.added && event.payload.added["+1"] >= 5` (the `in` check first: a missing kind fails the
  rule). Previews are limited to 2/s per user (burst 5, per replica). A user holds at most 100
  rules and 50 channels.
- The worker role routes each new event once (`FOR UPDATE SKIP LOCKED`): every enabled rule of the watch's
  owner now, on that watch or on all watches, is evaluated; a match adds one inbox item and one delivery
  per channel. A filter failing on an event counts as no match (`gedour_rule_eval_errors_total`).
  A rule's `priority` is `normal` or `high`: an event taken by a high rule of a user is high in that user's
  inbox, with a badge and a "High priority" filter, for example on `len(event.payload.cves) > 0`. The level is
  set once at routing: editing a rule leaves existing items as they are. The list stays newest first.
  Events stored before this version are not routed.
- Deliveries retry after 30 s, 2 min, 10 min and 30 min; the 5th failure is final (`dead`). A failed or
  dead delivery can be retried by hand. Lease and backoff use the database clock. A delivery whose
  channel owner no longer owns the watch is not sent (`dead`). On SIGTERM, sends in flight get
  `GEDOUR_SHUTDOWN_TIMEOUT`; past it they are cancelled and released, not counted as attempts.
- Live test: `POST /api/v1/rules/{id}/test {event_id}` sends an event of the saved rule's scope (its watch,
  or any of yours) to each of its channels now, one after the other in channel id order, as routing would:
  same event, watch, rule name and priority (Web Push urgency included), the filter not checked. The summary
  reads `[test] ` first on every channel type; webhook bodies and script inputs carry `"test": true` and, as
  in a channel test, `delivery_id` 0 (`X-Gedour-Delivery: 0` on webhooks). No delivery is written, the inbox
  is untouched, a digest rule sends the single-event message at once and a Nextcloud Talk hold does not
  apply. The answer lists `{channel_id, channel_name, ok, error, logs, accepted}` per channel (`[]` without
  one); a script with no free sandbox fails its item only. An event out of scope, not yours or missing, or
  another user's rule, is a 404. Channel and rule tests count together: one test per 10 s per user (429 with
  `Retry-After`). Each live test is audited as `rule.tested` with the event id. In the UI, each row of a
  saved rule's preview ("Test against last 20 events") has a "Live test" button that lists these results.
- Webhooks: `POST` JSON `{delivery_id, event, watch, rule, sent_at}`, plus `"test": true` on a live test,
  with `X-Gedour-Signature: sha256=<hex HMAC-SHA256(secret, raw body)>`, `X-Gedour-Event`, `X-Gedour-Delivery`. 2xx is success;
  redirects are not followed; 10 s timeout. Same SSRF guard as collectors; a literal private IP is refused
  on save. A signing secret left empty is generated and returned once, in the create response.
- Telegram: `sendMessage` in HTML mode with the bot token as secret; a Silent channel sends
  `disable_notification: true`, which notifies without sound. Email: multipart text + HTML through
  the instance's SMTP server, at most 10 recipients per channel.
- Nextcloud Talk: see [below](#nextcloud-talk-channels).
- Digest: a rule's `delivery` is `immediate` (default) or `digest`, with `at`, 1 to 24 local times (`HH:MM`),
  and `tz`, an IANA time zone (default `UTC`). A digest rule's deliveries to Telegram, Nextcloud Talk, email
  and Web Push channels wait for the rule's next time; its webhook and script channels still get each event
  at once. At a scheduled time each channel sends one message listing its due deliveries, from every digest
  rule, grouped by watch, oldest first, cut on a whole line past the type's cap (Telegram 4096 bytes, Talk
  31000 characters, email 256 KiB or 1000 events) with "+N more" and the inbox link. Web Push sends one
  notification "N unread events" opening the inbox. Only events still unread in the owner's inbox are
  listed: one read or archived meanwhile ends `skipped` (shown as Skipped) with the reason, and an empty
  digest sends nothing. Silent applies. A high priority rule cannot be a digest (400). An event matched by an
  immediate (or high) rule and a digest rule on the same channel is sent once, at once; matched by two digest
  rules, it waits for the earlier of their next times. Editing a rule's times moves its waiting deliveries
  to the new next time; switching it to `immediate`, disabling it or deleting it sends them at once.
  Channels carry no delivery setting (`delivery`, `at` or `tz` in a channel config is a 400) and "Send test"
  sends one test event. Upgrading moves each digest channel's schedule to the rules targeting it, when all
  the digest channels of a rule have one schedule and the rule has no immediate Telegram, Talk, email or Web
  Push channel. Any other rule with a digest channel stays immediate and is named in a `migration_warning`
  log line: its events then go out at once on every channel. Waiting digest deliveries keep their time.
- Inbox live updates: `GET /api/v1/inbox/stream` (SSE). The router `NOTIFY`s `gedour_inbox`; each web
  replica holds one `LISTEN` connection. Streams send `event: inbox` with `{"unread": n}`, a comment every
  25 s, and `event: signed_out` when the session ends (heartbeats check the session without refreshing
  it: an open tab does not defeat the idle timeout). At most 10 streams per user and 2000 per replica.
- Metrics: `gedour_events_routed_total`, `gedour_rule_eval_errors_total`,
  `gedour_deliveries_total{channel_type,status}`, `gedour_delivery_duration_seconds{channel_type}`,
  `gedour_sse_subscribers`.

## Nextcloud Talk channels

A Talk channel posts each delivery into a conversation as a bot. No Nextcloud user account is involved.
Nextcloud 32 or later is required for gedour to stay under Nextcloud's 401 throttling (see below).

1. Pick a shared secret of 40 to 128 characters, e.g. `openssl rand -hex 32`.
2. On the Nextcloud server, create the bot. Talk 22.0.1 and later has a command for a
   bot with the `response` feature only and no URL:
   ```sh
   occ talk:bot:create -s <secret> gedour
   ```
   On older servers, install it instead. This form requires a URL that no other bot uses, and Talk POSTs
   a Join or Leave notice to it when the bot is enabled or disabled in a conversation. gedour has no
   receiver for it and needs none: a failed notice only raises the bot's error count in Talk. Use an https
   URL on a host you control, e.g. gedour's own:
   ```sh
   occ talk:bot:install --feature response gedour <secret> https://gedour.example.com/talk
   ```
3. Enable it in the conversation. `occ talk:bot:list` gives `<bot-id>`, the end of the conversation's
   link (`/call/<token>`) gives `<token>`:
   ```sh
   occ talk:bot:setup <bot-id> <token>
   ```
4. In gedour, create a Nextcloud Talk channel with the Nextcloud URL, the conversation token and the
   secret, then press "Send test". Turn on Silent to post without a notification: Talk creates none for that
   message.

- Delivery: `POST {url}/ocs/v2.php/apps/spreed/api/v1/bot/{token}/message` with `OCS-APIRequest: true`,
  `X-Nextcloud-Talk-Bot-Random` and `X-Nextcloud-Talk-Bot-Signature` (hex HMAC-SHA256 of the random value
  followed by the message). Body: `message` (Markdown), `referenceId` (SHA-256 of the delivery id, the same
  on a retry), `silent`. A message over 31000 characters is cut to its title and link; the title is shortened
  when needed to keep the link, and a URL over 30000 characters once encoded is always left out.
- Only a 201 is sent: a proxy or SSO page answering 200 is not a post. 401, 404 and 413 are final at once
  (`dead`, shown as Failed) with the reason; fix the channel, then retry the delivery by hand. Anything else,
  other 2xx, 400, 429, 5xx and timeouts included, follows the retry policy above. Redirects are not followed.
- Nextcloud throttles every bot of an IP after repeated 401s. A 401 pauses that channel for 30 minutes or until
  it is edited: its deliveries wait without using an attempt. One Talk request per channel runs at a time, about
  one message per `GEDOUR_WORKER_POLL`. "Send test" and a manual retry of the refused delivery stay direct.
  A repair made only on the Nextcloud side can wait up to 30 minutes unless the channel is edited.
- Nextcloud 32 or later is required for this to hold: it answers 429 past 10 failures in 30 minutes, and it takes
  11 broken channels on one IP to get there. Older servers slow bot posts past gedour's timeout at 7 failures in
  12 hours, which one broken channel reaches in about 3 hours.
- To exempt gedour's outgoing IP, add it to the Brute-force IP whitelist in Nextcloud's security settings; that
  removes the protection for this IP, for every login attempt from it.
- https is required, and http only for hosts in `GEDOUR_ALLOW_HTTP_HOSTS`. Same SSRF guard as webhooks: a
  Nextcloud on a private network needs its range in `GEDOUR_ALLOW_PRIVATE_NETS`.
- Upstream text cannot mention anyone: `@` is escaped in titles, details and links.

## Script channels

- A script (JavaScript, [goja](https://github.com/dop251/goja), 64 KiB at most, 50 per user) defines
  `function main(event, ctx)`. It sees `event` (`type`, `occurred_at`, `payload`, `summary`, `url`),
  `watch` (`id`, `name`, `kind`), `rule` (`id`, `name`), `delivery_id` (also on `ctx`), `test` (`true` on a
  rule's live test, `undefined` otherwise, also on `ctx`), `log(...)` and
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
  4 children at once (others wait); dry runs, channel and rule tests 2 more, a 429 when both are busy (a
  rule test fails that channel's item instead), and their child dies when the client goes away.
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

## Web Push channels

A Web Push channel shows a native browser notification on the owner's registered browsers (devices).

- In the browser: devices are managed in the Web Push channel form (new and edit), which lists them with
  rename and remove (removing asks first). Picking Web Push in a new channel form registers the current
  browser, named after it, when it is not a device yet: the browser asks for permission then, never on page
  load or on save. "Add this browser" in the form is the fallback after a dismissed prompt; add another
  browser by opening the form there. A blocked permission or a browser without Web Push shows why and
  registers nothing. On iPhone and iPad, Safari offers Web Push only to a site
  added to the Home Screen, which gedour does not support yet.
- Service worker: `/sw.js` shows the notification. A click focuses an open gedour tab and routes it in place
  to the event's inbox item without a reload, or opens a new tab.
- Devices: `POST /api/v1/push/devices {endpoint, keys: {p256dh, auth}, name, user_agent}` with the browser's
  `PushSubscription`, subscribed with the key from `GET /api/v1/push/key`. The endpoint passes the webhook
  rules: https (http only for `GEDOUR_ALLOW_HTTP_HOSTS`); a literal private IP is a 400 at registration unless
  `GEDOUR_ALLOW_PRIVATE_NETS` allows it; a hostname (`localhost` included) registers, and the SSRF guard refuses
  it at send time when it resolves to a private address; `p256dh` is a 65-byte P-256 point, `auth`
  16 bytes, both base64url. A known endpoint updates its device; registered by another user, the device moves
  to them and leaves the previous owner's channels. At most 20 devices per user. Endpoints and keys are sealed
  with `GEDOUR_ENCRYPTION_KEY` and never returned or audited. `GET /api/v1/push/devices` returns
  `endpoint_sha256` (hex SHA-256 of the endpoint) so a browser recognizes its own device.
- Config: `devices` is `"all"` (every device of the owner, later ones included) or a list of the owner's
  device ids; `silent` sets urgency `low` and the notification's `silent` option. No secret.
- Delivery: one delivery pushes to every targeted device (5 at a time, 10 s each). Urgency is `high` for an
  event of a high priority rule, `low` when silent, `normal` otherwise. A retry skips the devices that
  accepted. A device the push service answers 404 or 410 is removed (audited as `push_device.gone`); a
  permanent refusal (400, 401, 403, 413) skips that device for this attempt; a retry caused by another device
  tries it again. Once the others accepted, the delivery is `sent` with the refusals as its error. With no
  device left to send to, the delivery is `dead` with `no device`.
- Payload: JSON `{title, body, url, tag, silent}`, the Telegram text cut to fit 4 KB; `url` is
  `/inbox?event=<id>`. "Send test" pushes to the channel's devices (cut after 25 s) and answers `accepted`, the
  devices that took it; `ok` when at least one took it and none can retry.
- VAPID: a key pair generated at first start and sealed in the database. If it no longer opens (a changed
  encryption key), Web Push is unavailable: `GET /api/v1/push/key` answers 503 and the type cannot be picked.

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
| `GEDOUR_SMTP_ADDR`, `GEDOUR_SMTP_FROM`, `GEDOUR_SMTP_USERNAME`, `GEDOUR_SMTP_PASSWORD` | optional | optional | Worker sends deliveries, web sends channel and rule tests |

```sh
kubectl create secret generic gedour \
  --from-literal=GEDOUR_DATABASE_URL='postgres://gedour:...@db:5432/gedour' \
  --from-literal=GEDOUR_ENCRYPTION_KEY="$(openssl rand -base64 32)" \
  --from-literal=OIDC_ISSUER=https://sso.example.com/realms/example \
  --from-literal=OIDC_CLIENT_ID=gedour --from-literal=OIDC_CLIENT_SECRET='...'
```

Other settings (table in [Configuration](#configuration)) go into the ConfigMap from an overlay:
`GEDOUR_PUBLIC_URL` (web, required with OIDC), the `GEDOUR_OIDC_*` role settings (web), `GEDOUR_ALLOW_HTTP_HOSTS` and `GEDOUR_ALLOW_PRIVATE_NETS`
(both roles: web validates watch URLs, the worker fetches), `GEDOUR_WORKER_POLL` and
`GEDOUR_WORKER_CONCURRENCY` (worker), `GEDOUR_TELEGRAM_API_URL`, `GEDOUR_SMTP_TLS` and `GEDOUR_SMTP_PLAIN_NETS`
(both). Telegram bot tokens belong to channels, in the database. Never set `GEDOUR_DEV_USER` in a cluster.

```yaml
# kustomization.yaml of an overlay
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: gedour
resources:
  - https://github.com/gedour/gedour//deploy/k8s/base?ref=v1.0.0
components:
  - https://github.com/gedour/gedour//deploy/k8s/components/networkpolicy?ref=v1.0.0
images:
  - name: ghcr.io/gedour/gedour
    newTag: v1.0.0
configMapGenerator:
  - name: gedour
    behavior: merge
    literals:
      - GEDOUR_PUBLIC_URL=https://gedour.example.com
```

- Image: `ghcr.io/gedour/gedour`. Releases push `vX.Y.Z`, `X.Y`, `X` and `latest` (pre-releases: the
  tag itself plus `alpha`, `beta` or `rc`); every push to `develop` pushes `develop-<sha>-<run>`, the run
  number ordering them for image automation. The base pins `v1.0.0` through `images:`. Pin a full
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
from another namespace, or a webhook, Nextcloud Talk server, script fetch or watch target listens elsewhere than 443.

### Smoke test

`make k8s-smoke` builds the image, creates a kind cluster (`gedour-smoke`), deploys `deploy/k8s/test` (the
base plus a throwaway PostgreSQL, the fake upstream, dev mode and the network policy; test only), and
checks: gedour waits for PostgreSQL, started only afterwards, then 2 web and 2 worker pods are ready
without restarts, `/healthz` and `/livez` answer through both Services, `/mcp` with an API key (401 without, tools listed with), a script dry run
and a script over its memory cap (killed, pod alive), a watch run and script deliveries on the workers.
Then it brings up `compose.yaml` under a fresh project and checks the dev-mode UI, `/api/v1/me` and
`/metrics`. Cluster, compose project and the images the smoke built are removed at the end. Needs kind
v0.23.0 or later (its node runs Kubernetes 1.30+, for the `preStop` sleep action; found in `PATH` or
`KIND=<path>`, tested with v0.33.0), kubectl, Docker, Go, curl and jq. Ports:
`K8S_SMOKE_PORT`, `K8S_SMOKE_FAKE_PORT`, `COMPOSE_SMOKE_PORT`, `COMPOSE_SMOKE_METRICS_PORT`,
`COMPOSE_SMOKE_DB_PORT`.

## Upgrading

- Watch sharing is gone (migration 17). The upgrade deletes every watch share. Each user a watch was shared
  with loses their rules pinned to it, and their inbox items and deliveries for its events. The owner keeps
  everything. Each removed share leaves a system `watch_share.removed` audit entry, readable by the watch
  owner, with the user and permission.
- During a rolling update, pods of the previous version answer 500 on every route that reads watches
  (watches, inbox, rules, deliveries, script test events, audit, admin) until they are replaced, and their
  router and delivery loops fail; collection carries on. No event is lost: the new workers route what the
  old ones could not. A delivery an old worker loads in that window may spend one retry.
- Rollback: deploy the older image first. It starts on the migrated database but fails those same routes.
  Once no pod of the new version runs (web and worker), run migration 17's `Down` section by hand (`psql`)
  and delete version 17 from `goose_db_version`: a new-version pod starting before that would apply 17
  again. Shares come back as an empty table. Only a backup taken before the upgrade restores the deleted
  rules, inbox items and deliveries.
- Discussions and reactions (migration 20). Rollback: deploy the older image first; its `github_discussion`
  watches then fail every run and their detail page does not render. Delete those watches, run migration
  20's `Down` section by hand (`psql`; it refuses while such a watch exists) and delete version 20 from
  `goose_db_version`. Stored states keep the new keys, which the older image ignores; saving a watch from its
  UI drops the `new_discussions` and `reactions` boxes.
- Suspended API keys (migration 24). During a rolling update, pods of the previous version still accept a
  suspended key, and a missing-role refusal they handle revokes the keys as before. Rollback: scale web and worker
  to 0, run migration 24's `Down` section by hand (`psql`; it revokes every live suspended key), delete version 24
  from `goose_db_version`, then deploy the older image. Started earlier, the older image accepts suspended keys.

## Endpoints

- `GET /healthz`: 200 when PostgreSQL answers, 503 otherwise.
- `GET /livez` (metrics port only): 200 while the process serves, whatever PostgreSQL does.
- `POST /mcp`: MCP server, API key only (see [MCP](#mcp)); `GET` and `DELETE` answer 405.
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
  A user sees what they did or what was done as them, plus entries on the watches, credentials, channels, rules
  and deliveries (through the channel) they own now, and the push device entries that name them as owner (a move
  writes one per owner, naming that owner only). `watch_share` entries from before migration 17, the
  upgrade's system entries included, go to the watch owner. `GET /api/v1/audit/actors`: the actors of those entries.
- `/api/v1/watches` (`GET ?kind=&q=`, `POST`), `/api/v1/watches/{id}` (`GET`,
  `PATCH`, `DELETE`), `POST /api/v1/watches/{id}/pause|resume`. A watch carries its run status:
  `next_run_at`, `running` (true while a worker holds the claim), `last_run_at`, `last_status` (`ok`, `error`,
  `rate_limited`), `last_error`, `consecutive_failures`.
- `GET /api/v1/watches/{id}/events?cursor=&limit=`: events, newest first. `GET /api/v1/watches/{id}/states?limit=`:
  state history, latest first. `POST /api/v1/watches/{id}/run` (409 when paused):
  runs at the next poll. Resume also runs at once. `POST /api/v1/watches/run` does the same for every unpaused
  watch the caller owns and answers `{scheduled}`.
- Watches are private to their owner: another user's watch answers 404 on every watch route, as an unknown one does.
- `/api/v1/credentials` (`GET`, `POST`), `/api/v1/credentials/{id}` (`GET`, `DELETE`, 409 while a watch uses
  it), `PUT /api/v1/credentials/{id}/secret`. Secrets are write-only; responses carry `last4` (empty for
  secrets under 16 characters). Credentials are private to their owner.

- `/api/v1/api-keys` (`GET`, `POST {name, scope, duration}`, 409 at 10 active keys or while acting as a user),
  `DELETE /api/v1/api-keys/{id}` (204, again on a revoked key; 404 for another user's). Only the `POST` answer holds the key.

- `/api/v1/channels` (`GET`, `POST`), `/api/v1/channels/{id}` (`GET`, `PATCH`, `DELETE`, 409 while a rule
  uses it), `POST /api/v1/channels/{id}/test` (one test per 10 s per user, rule tests included, 429 otherwise), `GET /api/v1/channel-types`.
  Secrets are write-only (`last4` only).
- `/api/v1/rules` (`GET`, `POST`), `/api/v1/rules/{id}` (`GET`, `PATCH`, `DELETE`),
  `POST /api/v1/rules/preview {watch_id, filter}`: the filter on the last 20 events of your watches in scope.
  `POST /api/v1/rules/{id}/test {event_id}`: [live test](#routing-and-channels), in the channel tests' 10 s limit.
  `GET /api/v1/filters/reference`: what a filter may use, and its limits.
- `GET /api/v1/deliveries?channel_id=&status=&cursor=`, `POST /api/v1/deliveries/{id}/retry`.
- `GET /api/v1/push/key`, `/api/v1/push/devices` (`GET`, `POST`), `/api/v1/push/devices/{id}` (`PATCH {name}`,
  `DELETE`): see [Web Push channels](#web-push-channels). Another user's device is a 404.
- `/api/v1/scripts` (`GET` without sources, `POST`), `/api/v1/scripts/{id}` (`GET`, `PATCH`, `DELETE`),
  `POST /api/v1/scripts/{id}/dry-run` and `POST /api/v1/scripts/dry-run {source}`, each with
  `{event_id}` (an event of one of your watches) or `{sample: {type, payload, occurred_at, watch}}`: answers
  `{ok, outcome, error, logs, result, fetches, duration_ms}`.
- `GET /api/v1/inbox?state=unread|all|archived&priority=normal|high&cursor=`, `GET /api/v1/inbox/count`,
  `POST /api/v1/inbox/read|unread|archive|unarchive {ids}` (the caller's items only; `unread` also unarchives), `GET /api/v1/inbox/stream`.
- Channels, rules, scripts and deliveries are private to their owner (404 for others); their changes, channel
  and rule tests and delivery retries are audited. Inbox items and deliveries of a watch the user no longer
  owns are hidden, and a rule's `watch_name` is null then.

- `GET /api/v1/admin/users`, `POST /api/v1/admin/users/{id}/disable|enable` (204; 404 unknown account; 409
  on your own account, a purged one, or one already in the asked state). Every `/api/v1/admin/` call needs
  an admin session (403 otherwise).

The full contract is `api/openapi.json` (also served at `/api/openapi.json`).

Every other `/api` route needs a session (401 otherwise).

## Development

See `AGENTS.md` for build, test, lint and e2e commands.

## License

Apache License 2.0, see [`LICENSE`](./LICENSE) and [`NOTICE`](./NOTICE).

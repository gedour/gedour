# 016 — Credentials over MCP

**Status**: shipped
**Requested by**: babs
**Date**: 2026-11-11

## Problem

An MCP client can create and edit watches but cannot give them a credential. A private repository, a
GHES or self-hosted GitLab watch, or a rate-limited GitHub watch has to be finished in the UI. The
client cannot even see which credentials exist. Spec 007 forbids all of this because an injected prompt
could send the owner's token to a host of its choosing.

## Solution

A `manage` key can list the owner's credentials (id, name, kind) and attach, swap or detach one on
`create_watch` and `update_watch`. A key can send a credential only to a target that credential already
reaches in one of the owner's watches. A credential no watch uses yet can go only to its kind's default
host. A first use on a custom host stays a UI step. Under the same rule, a key can now change the config
of a watch that has a credential.

## Scope

- New MCP tool `list_credentials` (`list-credentials`, scope `manage`, read-only): returns `id`, `name`,
  `kind` only. `kind` filter kept.
- `create_watch` and `update_watch` expose `credential_id` (no longer `hidden`). `update_watch` keeps
  "empty string detaches".
- In `createWatch`/`updateWatch`, when the call comes through a key, replace `keyCredentialError` with
  the **credential target check**: the watch's resulting target (scheme, host and path) must equal the
  target of another owner's watch that has the same `credential_id`. A credential no watch holds, the
  edited one included, may also go to the kind's `defaultHosts` over https.
- The check runs when a key's call ends with a credential on the watch and either the credential or the
  target changes. That covers attach, swap, and a config change on a watch that has a credential, which
  replaces the current "cannot change the config of a watch that has a credential" 403 in `updateWatch`.
- The existing guards stay and run first: owned credential, kind family (`checkCredential`), known host,
  key cap, rate-limit hold, budget.
- Spec 007, README (MCP section, scope table, tool table, "No retarget" bullet, redirect paragraph) and
  `AGENTS.md:114` are amended to match.

## Out of scope

- Creating, rotating or deleting a credential over MCP. Secret material never crosses MCP.
- `last4`, `username`, `used_by_count`, timestamps in `list_credentials`.
- Any change to session (UI) calls: the target check applies to keys only.
- A credential exposed to `read` or `write` keys.

## Acceptance criteria

- [x] **AC1** A `manage` key's `tools/list` has `list_credentials`; a `read` or `write` key's does not, and calling it gets `-32602`.
- [x] **AC2** `list_credentials` returns each of the owner's credentials as `{id, name, kind}` exactly, never another user's, and never `last4`, `username` or a secret.
- [x] **AC3** `create_watch` and `update_watch` schemas list `credential_id`.
- [x] **AC4** Given credential C used by a watch on target T, a key's `create_watch` on target T with C succeeds.
- [x] **AC5** Given credential C used only on target T, a key's `create_watch` with C on another known target T2 (another saved host, or the kind's default host) gets a 403 naming `body.credential_id`.
- [x] **AC6** Given credential C used by no watch, a key's `create_watch` with C on the kind's default host succeeds, and on a saved custom host gets a 403.
- [x] **AC7** A key's `update_watch` attaching or swapping in C follows AC4 to AC6, counting only the other watches, not the one being updated.
- [x] **AC8** A key's `update_watch` changing the config of a watch that has credential C succeeds if the new target passes the check, and gets a 403 otherwise. The watch row is unchanged after a 403.
- [x] **AC9** A key's `update_watch` with `credential_id: ""` detaches the credential.
- [x] **AC10** A credential of another user, or of the wrong kind family, still gets the existing 400 through a key.
- [x] **AC11** Session (UI) calls attach any owned credential of the right kind to any target, as before.
- [x] **AC12** Spec 007, README and `AGENTS.md` no longer say a key cannot attach a credential or change a credentialed watch's config. `go test ./pkg/server -run TestDocs` is green.

## Phases

### Phase 1 — Credential target check in the watch handlers
- Work: `checkCredentialTarget(ctx, tx, owner, kind, credID, exceptWatchID, cfg)` in `pkg/watch`; wire it
  into `createWatch`/`updateWatch` for key calls; drop `keyCredentialError` and that 403.
- **Covers**: AC4, AC5, AC6, AC7, AC8, AC9, AC10, AC11
- **Data model impact**: none
- **DoD**: `go test ./pkg/watch ./pkg/server` green, with a REST-through-key test per AC listed.

### Phase 2 — MCP surface and docs
- Work: `list_credentials` row with a shape keeping `id`, `name`, `kind`; remove `hidden: credential_id`
  from both watch rows and update their `desc`; amend spec 007, README, `AGENTS.md`.
- **Covers**: AC1, AC2, AC3, AC12
- **Data model impact**: none
- **DoD**: `go test ./pkg/mcpserver ./pkg/server` green, including `TestDocs`; `golangci-lint run` clean.

## Data model impact (summary)

None. `watches.credential_id` and `credentials` are unchanged.

## Open questions

- [x] Hosts a key may send a credential to: those it already reaches, else the kind's default. (babs, 2026-11-11)
- [x] Config change on a credentialed watch through a key: allowed under the same check. (babs, 2026-11-11)

## Decisions

- `list_credentials` is `manage`, not `read`: only the manage tools consume a credential id. A `read` key still sees `credential_id` on watches, as today.
- Target means scheme, host and path (`comparableTarget` plus the scheme), not host alone: a credential reaching `https://H` must not go to `http://H`.
- On update, the watch being edited counts as a use of its credential but not as a target the credential reaches. Otherwise its old target would vouch for its new one, and its only credential would count as unused.
- Detach through a key is allowed with no check: it sends nothing anywhere.
- The credential row is read `FOR KEY SHARE` (already in `checkCredential`) so a concurrent delete cannot slip between the check and the write, and a key call also takes `FOR NO KEY UPDATE` so two key calls on one credential serialize.
- Detaching a credential, or deleting its watches, makes it unused, so a key may then attach it on the kind's default host. Accepted: default hosts are vendor-run, nobody else reads what gedour sends them.
- A `manage` key can read the owner's private repositories at a target a credential already reaches, or with an unused credential at its kind's default host: target is scheme, host and path, and the repository is not part of it. Accepted by babs when choosing "same rule" for config changes (2026-11-11).
- `list_credentials` drops `used_by_count` because a client does not need it to attach a credential.

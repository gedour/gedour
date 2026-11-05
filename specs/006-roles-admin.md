# 006 — Roles from the identity provider and account administration

**Status**: shipped
**Requested by**: babs
**Date**: 2026-11-05

## Problem

Anyone the identity provider authenticates gets into gedour: there is no way to limit access to a
subset of the provider's users. Nobody can administer the instance either: accounts of people who left
keep their watches, channels and credentials forever, and a broken config (failing channel, bad rule)
can only be fixed by its owner.

## Solution

gedour reads a roles claim from the ID token, the same way for Microsoft Entra ID (app roles) and
Keycloak (client roles mapped to a flat claim). An operator can require a user role to sign in, and
name an admin role. An admin gets an Administration page listing every account with its last sign-in
and how much it owns. From there the admin can disable an account (reversible), purge a disabled one
(its data is deleted, the account row is kept anonymized for the audit trail), hand its shared watches
to another user first, and act as any user to fix their configuration through the normal pages, with
a banner and every change audited under the admin's name.

## Scope

- Config:
  - `GEDOUR_OIDC_ROLES_CLAIM` (default `roles`); a dotted path reaches a nested claim
    (`realm_access.roles`).
  - `GEDOUR_OIDC_REQUIRED_ROLE`: empty keeps today's behaviour (any authenticated user).
  - `GEDOUR_OIDC_ADMIN_ROLE`: empty means no admin.
  - The admin role grants sign-in even without the required role.
- Roles read from the verified ID token at login only; the session stores the admin flag.
- Refused sign-in (missing role, disabled account) lands on the sign-in gate with a new reason
  `forbidden`. No session is created and an audit entry is written.
- Admin API and page (admin only, else 403), listing every account: email, name, created,
  last sign-in, status (active, disabled, purged), counts of watches (shared ones apart), channels,
  rules, scripts and credentials. Sort by last sign-in, oldest first.
- Disable: sign-in refused, every session revoked at once, the account's rules route nothing and it
  gets no inbox items. Its data and its watches stay; shared watches keep collecting for the other
  users. Re-enable undoes it.
- Transfer: the admin moves an owned watch to another user. The credential it uses moves with it when
  no other watch of the old owner uses that credential; otherwise the transfer is refused with the
  list of the watches that share it. A share the new owner held on that watch is dropped.
- Purge (disabled accounts only): one transaction deletes the account's watches, channels, rules,
  scripts, credentials, shares and inbox, blanks `email` and `display_name`, and marks the row
  purged. The audit log is untouched. Watches still shared with others are listed in the
  confirmation dialog.
- Act as: the admin switches the session to a target user. Every page and API call then runs as that
  user, a banner names them with a "Stop" button, and audit entries keep the admin as actor plus the
  target as `on_behalf_of`. The admin cannot act as themselves or as a purged account.
- Dev mode: the dev user is admin.
- README: Entra app roles setup (App registration → App roles, Enterprise app → assign users or
  groups) and Keycloak setup (client role + "User Client Role" mapper, claim `roles`, multivalued,
  added to the ID token); the compose Keycloak realm gets the mapper, a user role and an admin role.

## Out of scope

- Automatic purge or disable after N days of inactivity.
- Hard deletion of the `users` row (blocked by the append-only audit log).
- Roles from groups (`groups` claim, Entra group overage, Graph calls).
- Role changes taking effect before the next sign-in (except disable, which revokes sessions).
- Finer admin permissions (read-only admin, per-resource rights).
- Admin edits of other users' data outside act-as.
- Restoring a purged account's data.

## Acceptance criteria

- [x] **AC1** Given `GEDOUR_OIDC_REQUIRED_ROLE=gedour-user`, a user whose ID token lacks it is sent to
  the sign-in gate with reason `forbidden`, gets no session, and a `user.login_denied` audit entry is
  written.
- [x] **AC2** Given the required role is unset, any authenticated user signs in as today.
- [x] **AC3** The roles claim is read as a flat array (`roles`, Entra and Keycloak with mapper) and
  through a dotted path (`realm_access.roles`); a missing claim, a non-array value or a non-string
  item means no role, never an error page.
- [x] **AC4** A user holding the admin role and not the required role signs in, and `/api/v1/me` reports
  `is_admin: true`; any other user gets `false`.
- [x] **AC5** A non-admin calling any `/api/v1/admin/*` operation gets 403, and the Administration entry
  is not shown in the navigation.
- [x] **AC6** The admin account list shows every account with last sign-in, status and per-type
  counts, oldest sign-in first, never-signed-in accounts at the top.
- [x] **AC7** Disabling an account revokes all its sessions (its next API call gets 401) and its next
  sign-in is refused with reason `forbidden`.
- [x] **AC8** While an account is disabled, its rules produce no deliveries and it receives no inbox
  items, while a watch it shares keeps producing events for the other users.
- [x] **AC9** Re-enabling an account restores sign-in and routing; no data was lost.
- [x] **AC10** Transferring a watch moves ownership and its credential to the target user. The watch
  keeps collecting, and the old owner loses access unless it was shared back.
- [x] **AC11** A transfer is refused (409, watch names listed) when the credential is also used by
  another watch of the old owner; nothing changes.
- [x] **AC12** Purge is refused (409) on an active account.
- [x] **AC13** Purging a disabled account deletes its watches, channels, rules, scripts, credentials,
  shares and inbox items in one transaction, blanks its email and name, keeps every audit row, and
  the account shows as purged.
- [x] **AC14** A purged account signing in again is refused with reason `forbidden`.
- [x] **AC15** While acting as a user, `/api/v1/me` returns the target user plus the admin's id, every
  page shows that user's data, and a banner offers "Stop".
- [x] **AC16** A mutation made while acting as a user writes an audit entry with the admin as actor and
  the target as `on_behalf_of`, visible in the target user's audit view.
- [x] **AC17** Act-as is refused for oneself and for a purged account; stopping returns the session to
  the admin's own data.
- [x] **AC18** Every admin action (disable, enable, transfer, purge, act-as start/stop) is audited
  with before and after state.
- [x] **AC19** In dev mode the dev user is admin.

## Phases

### Phase 1 — Roles at sign-in
- Work: the three config vars and their validation; claim extraction (flat and dotted); the required
  role check in the callback; `forbidden` gate reason (backend + `SignInGate` copy); `is_admin` on the
  session and in `/me`; dev user is admin; compose realm mapper and roles; README.
- **Covers**: AC1, AC2, AC3, AC4, AC19
- **Data model impact**: `sessions.is_admin boolean NOT NULL DEFAULT false`.
- **DoD**: `make test` green with the claim cases of AC3 table-tested; `make e2e` green with a Keycloak
  user lacking the role refused at the gate; `make openapi` clean.

### Phase 2 — Account list, disable, enable
- Work: `admin` guard (403); `GET /api/v1/admin/users`; `POST …/users/{id}/disable|enable`; session
  revocation; disabled check at sign-in; router and inbox skip disabled owners; Administration page
  with the list and the two actions.
- **Covers**: AC5, AC6, AC7, AC8, AC9, AC18 (disable/enable)
- **Data model impact**: `users.disabled_at timestamptz`, `users.purged_at timestamptz`.
- **DoD**: Go DB tests for AC7–AC9 (routing skip asserted on the router); e2e: admin disables bob, bob's
  session drops to the gate, re-enable lets bob back in.

### Phase 3 — Transfer and purge
- Work: `POST …/watches/{id}/transfer`; `POST …/users/{id}/purge`; confirmation dialog listing shared
  watches; purged status in the list and at sign-in.
- **Covers**: AC10, AC11, AC12, AC13, AC14, AC18 (transfer/purge)
- **Data model impact**: none beyond phase 2.
- **DoD**: Go DB tests for AC10–AC14, including a purge of an account owning a channel used by a rule
  (`rule_channels` RESTRICT inside the cascade) and audit rows surviving; e2e: transfer then purge.

### Phase 4 — Act as
- Work: `POST …/users/{id}/act-as` and `DELETE /api/v1/admin/act-as`; the session guard resolves the
  effective user; `/me` exposes `acting_as`; `audit.Record` writes `on_behalf_of`; banner with "Stop";
  audit view shows "by <admin> for <user>".
- **Covers**: AC15, AC16, AC17, AC18 (act-as)
- **Data model impact**: `sessions.acting_as uuid REFERENCES users (id) ON DELETE SET NULL`;
  `audit_log.on_behalf_of uuid REFERENCES users (id)`.
- **DoD**: Go tests for AC15–AC17; e2e: admin acts as bob, edits a rule, bob's audit view shows the entry
  with both names; `make coverage` passes its floors.

## Data model impact (summary)

- `sessions`: `is_admin boolean NOT NULL DEFAULT false`, `acting_as uuid` (FK users, `ON DELETE SET NULL`).
- `users`: `disabled_at timestamptz`, `purged_at timestamptz`.
- `audit_log`: `on_behalf_of uuid` (FK users). Adding a column does not fire the append-only triggers.

## Open questions

- [ ] None.

## Decisions

- Roles over groups: Entra `groups` carries GUIDs and overflows past 200 groups; app roles and Keycloak
  client roles both yield a flat string array.
- Admin flag read at sign-in only, kept on the session: an admin role removed in the IdP stays
  effective until the session ends (`GEDOUR_SESSION_TTL`, default 168h). Disable is the immediate lever.
- A purged row keeps `oidc_sub` so the same person stays refused; email and name are blanked.
- A disabled account's watches keep collecting: stopping them would silently break other users' shares.
- Credentials move with a transferred watch; the secret's AAD is the credential id, not the owner, so
  it still decrypts. A credential name clash at the new owner gets a ` (transferred)` suffix.
- Act-as exposes the target's inbox to the admin; accepted for a self-hosted admin tool, made visible
  by the banner and the `on_behalf_of` audit trail.
- The admin role alone grants sign-in, so an admin cannot lock themselves out by missing the user role.

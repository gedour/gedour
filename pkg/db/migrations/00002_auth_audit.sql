-- Users, PG-backed sessions and the append-only audit log.
-- +goose Up
CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    oidc_sub      text NOT NULL UNIQUE,
    email         text NOT NULL DEFAULT '',
    display_name  text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz
);

-- id is sha256(cookie token): a database leak does not hand out live sessions.
CREATE TABLE sessions (
    id           bytea PRIMARY KEY,
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    csrf_token   text NOT NULL,
    -- Raw ID token, sent back as id_token_hint on sign-out; never returned by the API.
    id_token     text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL
);
CREATE INDEX sessions_user_id_idx ON sessions (user_id);
-- Both expiry predicates of the purge (expires_at, last_seen_at) are index-backed.
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);
CREATE INDEX sessions_last_seen_at_idx ON sessions (last_seen_at);

CREATE TABLE audit_log (
    id            bigserial PRIMARY KEY,
    at            timestamptz NOT NULL DEFAULT now(),
    actor_user_id uuid REFERENCES users (id),
    action        text NOT NULL CHECK (action <> ''),
    target_type   text NOT NULL CHECK (target_type <> ''),
    target_id     text NOT NULL DEFAULT '',
    before        jsonb,
    after         jsonb,
    request_id    text NOT NULL DEFAULT ''
);
-- id breaks ties between rows of one transaction (same now()) for keyset pagination.
CREATE INDEX audit_log_at_idx ON audit_log (at DESC, id DESC);
CREATE INDEX audit_log_actor_idx ON audit_log (actor_user_id, at DESC, id DESC);
CREATE INDEX audit_log_target_type_idx ON audit_log (target_type, at DESC, id DESC);

-- +goose StatementBegin
CREATE FUNCTION audit_log_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_log is append-only';
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER audit_log_append_only BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_append_only();
CREATE TRIGGER audit_log_no_truncate BEFORE TRUNCATE ON audit_log
    FOR EACH STATEMENT EXECUTE FUNCTION audit_log_append_only();

-- +goose Down
DROP TABLE audit_log;
DROP FUNCTION audit_log_append_only();
DROP TABLE sessions;
DROP TABLE users;

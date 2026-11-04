-- Watches, their encrypted credentials and per-user shares.
-- +goose Up
CREATE TABLE credentials (
    id                uuid PRIMARY KEY,
    owner_id          uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name              text NOT NULL CHECK (name <> ''),
    kind              text NOT NULL CHECK (kind IN ('github_token', 'gitlab_token', 'registry_basic')),
    username          text,
    -- AES-256-GCM under GEDOUR_ENCRYPTION_KEY, AAD = id: a ciphertext copied to another row fails to open.
    secret_ciphertext bytea NOT NULL,
    secret_nonce      bytea NOT NULL,
    key_version       smallint NOT NULL,
    last4             text NOT NULL DEFAULT '',
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (owner_id, name)
);

CREATE TABLE watches (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name          text NOT NULL CHECK (name <> ''),
    kind          text NOT NULL CHECK (kind IN ('github_release', 'github_pr', 'gitlab_release', 'gitlab_mr', 'oci_tags')),
    config        jsonb NOT NULL,
    credential_id uuid REFERENCES credentials (id) ON DELETE RESTRICT,
    schedule      text NOT NULL DEFAULT '15m',
    paused        boolean NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX watches_owner_id_idx ON watches (owner_id);
CREATE INDEX watches_credential_id_idx ON watches (credential_id);

CREATE TABLE watch_shares (
    watch_id   uuid NOT NULL REFERENCES watches (id) ON DELETE CASCADE,
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    permission text NOT NULL CHECK (permission IN ('ro', 'rw')),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (watch_id, user_id)
);
-- "Shared with me" and the audit visibility both look shares up by user.
CREATE INDEX watch_shares_user_id_idx ON watch_shares (user_id);

-- +goose StatementBegin
CREATE FUNCTION watch_shares_not_owner() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM watches WHERE id = NEW.watch_id AND owner_id = NEW.user_id) THEN
        RAISE EXCEPTION 'a watch cannot be shared with its owner' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER watch_shares_not_owner BEFORE INSERT OR UPDATE ON watch_shares
    FOR EACH ROW EXECUTE FUNCTION watch_shares_not_owner();

-- +goose Down
DROP TABLE watch_shares;
DROP FUNCTION watch_shares_not_owner();
DROP TABLE watches;
DROP TABLE credentials;

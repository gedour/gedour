-- API keys open /mcp for one user; only the SHA-256 of a key is stored.
-- +goose Up
CREATE TABLE api_keys (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name         text NOT NULL,
    hash         bytea NOT NULL UNIQUE,
    prefix       text NOT NULL,
    scope        text NOT NULL CHECK (scope IN ('read', 'write', 'manage')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz, -- null: a `never` key
    last_used_at timestamptz,
    revoked_at   timestamptz
);
CREATE INDEX api_keys_user_id_idx ON api_keys (user_id);

-- +goose Down
DROP TABLE api_keys;

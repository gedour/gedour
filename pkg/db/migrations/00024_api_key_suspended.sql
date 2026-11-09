-- A missing_role refusal suspends a key until the next successful sign-in (007-AC12), instead of revoking it.
-- A nullable column without default is a catalog change: no table rewrite, no scan. The exclusive lock waits at most lock_timeout (migrationLockTimeout).
-- Old pods ignore the column: until they are gone, a suspended key still answers on them.
-- +goose Up
ALTER TABLE api_keys ADD COLUMN suspended_at timestamptz;

-- +goose Down
-- Older code cannot read a suspension: it becomes the revocation it replaced. Until this runs, older code accepts the key.
-- A sign-in since the suspension (on older code, which never lifts it) passed the role check: that key stays.
UPDATE api_keys k SET revoked_at = k.suspended_at WHERE k.suspended_at IS NOT NULL AND k.revoked_at IS NULL
    AND (k.expires_at IS NULL OR k.expires_at > now())
    AND NOT EXISTS (SELECT 1 FROM users u WHERE u.id = k.user_id AND u.last_login_at > k.suspended_at);
ALTER TABLE api_keys DROP COLUMN suspended_at;

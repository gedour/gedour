-- A missing_role refusal suspends a key until the next successful sign-in (007-AC12), instead of revoking it.
-- A nullable column without default is a catalog change: no table rewrite, no scan. The exclusive lock waits at most lock_timeout (migrationLockTimeout).
-- Old pods ignore the column: until they are gone, a suspended key still answers on them.
-- +goose Up
ALTER TABLE api_keys ADD COLUMN suspended_at timestamptz;

-- +goose Down
-- Older code cannot read a suspension: it becomes the revocation it replaced. Every live suspended key goes, even one
-- whose owner signed in since on older code: suspended_at keeps the first refusal, so it cannot rule out a later one.
UPDATE api_keys SET revoked_at = suspended_at WHERE suspended_at IS NOT NULL AND revoked_at IS NULL
    AND (expires_at IS NULL OR expires_at > now());
ALTER TABLE api_keys DROP COLUMN suspended_at;

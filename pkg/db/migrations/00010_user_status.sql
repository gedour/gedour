-- Account status: disabled (reversible, signs in to nothing, routes nothing) and purged (data deleted, row kept for the audit log).
-- +goose Up
ALTER TABLE users ADD COLUMN disabled_at timestamptz, ADD COLUMN purged_at timestamptz;

-- +goose Down
ALTER TABLE users DROP COLUMN disabled_at, DROP COLUMN purged_at;

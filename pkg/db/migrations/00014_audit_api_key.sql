-- Which API key made a change. No FK: a purge deletes keys and the audit log outlives them.
-- A nullable column without default is a catalog change: no table rewrite, no scan, and adding a column fires no row trigger.
-- Old pods keep working: their INSERT names its columns and leaves this one null. The exclusive lock waits at most lock_timeout (migrationLockTimeout).
-- +goose Up
ALTER TABLE audit_log ADD COLUMN api_key_id uuid;

-- +goose Down
ALTER TABLE audit_log DROP COLUMN api_key_id;

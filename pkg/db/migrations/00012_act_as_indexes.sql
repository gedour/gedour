-- Outside a transaction: VALIDATE and CREATE INDEX CONCURRENTLY let writes go on while they scan.
-- A build cut short leaves an INVALID index of the name: the DROP before each build makes a retry redo it.
-- +goose NO TRANSACTION
-- +goose Up
ALTER TABLE audit_log VALIDATE CONSTRAINT audit_log_on_behalf_of_fkey;
DROP INDEX CONCURRENTLY IF EXISTS audit_log_on_behalf_of_idx;
CREATE INDEX CONCURRENTLY audit_log_on_behalf_of_idx ON audit_log (on_behalf_of, at DESC, id DESC) WHERE on_behalf_of IS NOT NULL;
DROP INDEX CONCURRENTLY IF EXISTS sessions_acting_as_idx;
CREATE INDEX CONCURRENTLY sessions_acting_as_idx ON sessions (acting_as) WHERE acting_as IS NOT NULL;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS sessions_acting_as_idx;
DROP INDEX CONCURRENTLY IF EXISTS audit_log_on_behalf_of_idx;

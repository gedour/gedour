-- The delivery claim finds a poll's events by watch and observed_at (017); without it, each due row seq-scans events.
-- Outside a transaction: CREATE INDEX CONCURRENTLY lets writes go on while it scans.
-- A build cut short leaves an INVALID index of the name: the DROP before the build makes a retry redo it.
-- +goose NO TRANSACTION
-- +goose Up
DROP INDEX CONCURRENTLY IF EXISTS events_watch_observed_idx;
CREATE INDEX CONCURRENTLY events_watch_observed_idx ON events (watch_id, observed_at);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS events_watch_observed_idx;

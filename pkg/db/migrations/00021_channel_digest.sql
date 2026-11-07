-- Channel digests (012): deliveries waiting for a digest, and the skipped status.
-- The new check is NOT VALID here and validated in 00022, so this transaction never scans deliveries
-- under its exclusive lock.
-- +goose Up
-- true: queued for the channel's next digest; false: sent alone (immediate channel or high priority bypass).
ALTER TABLE deliveries ADD COLUMN digest boolean NOT NULL DEFAULT false;
ALTER TABLE deliveries DROP CONSTRAINT deliveries_status_check;
ALTER TABLE deliveries ADD CONSTRAINT deliveries_status_check CHECK (status IN ('pending', 'sent', 'failed', 'dead', 'skipped')) NOT VALID;

-- +goose Down
-- A skipped delivery was never sent and has no retry left: dead is the closest older status.
UPDATE deliveries SET status = 'dead' WHERE status = 'skipped';
ALTER TABLE deliveries DROP CONSTRAINT deliveries_status_check;
ALTER TABLE deliveries ADD CONSTRAINT deliveries_status_check CHECK (status IN ('pending', 'sent', 'failed', 'dead'));
ALTER TABLE deliveries DROP COLUMN digest;

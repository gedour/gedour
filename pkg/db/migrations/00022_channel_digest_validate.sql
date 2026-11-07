-- Outside a transaction: VALIDATE scans deliveries under SHARE UPDATE EXCLUSIVE, so writes go on meanwhile.
-- +goose NO TRANSACTION
-- +goose Up
ALTER TABLE deliveries VALIDATE CONSTRAINT deliveries_status_check;

-- +goose Down

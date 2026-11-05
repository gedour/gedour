-- Act as a user: the admin's session runs as acting_as; entries written meanwhile name that user in on_behalf_of.
-- Adding a column fires no row trigger, so the append-only guard on audit_log stays quiet. The audit FK is
-- NOT VALID here and validated in 00012, so this transaction never scans the log under its exclusive lock.
-- +goose Up
ALTER TABLE sessions ADD COLUMN acting_as uuid REFERENCES users (id) ON DELETE SET NULL;
ALTER TABLE audit_log ADD COLUMN on_behalf_of uuid;
ALTER TABLE audit_log ADD CONSTRAINT audit_log_on_behalf_of_fkey FOREIGN KEY (on_behalf_of) REFERENCES users (id) NOT VALID;

-- +goose Down
ALTER TABLE audit_log DROP COLUMN on_behalf_of;
ALTER TABLE sessions DROP COLUMN acting_as;

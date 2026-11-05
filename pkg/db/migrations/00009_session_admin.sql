-- Admin flag read from the identity provider's roles at sign-in; older sessions are not admin.
-- +goose Up
ALTER TABLE sessions ADD COLUMN is_admin boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE sessions DROP COLUMN is_admin;

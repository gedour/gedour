-- Rule priority, and the level it gave each inbox item at routing.
-- +goose Up
ALTER TABLE rules ADD COLUMN priority text NOT NULL DEFAULT 'normal' CHECK (priority IN ('normal', 'high'));
ALTER TABLE inbox_items ADD COLUMN priority text NOT NULL DEFAULT 'normal' CHECK (priority IN ('normal', 'high'));
CREATE INDEX inbox_items_user_high_idx ON inbox_items (user_id, id DESC) WHERE priority = 'high';

-- +goose Down
DROP INDEX inbox_items_user_high_idx;
ALTER TABLE inbox_items DROP COLUMN priority;
ALTER TABLE rules DROP COLUMN priority;

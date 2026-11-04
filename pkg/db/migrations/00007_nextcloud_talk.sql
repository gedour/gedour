-- The Nextcloud Talk channel type; url, conversation and silent live in the JSON config.
-- +goose Up
ALTER TABLE channels DROP CONSTRAINT channels_type_check;
ALTER TABLE channels ADD CONSTRAINT channels_type_check CHECK (type IN ('telegram', 'nextcloud_talk', 'email', 'webhook', 'script'));

-- +goose Down
DELETE FROM rule_channels WHERE channel_id IN (SELECT id FROM channels WHERE type = 'nextcloud_talk');
DELETE FROM channels WHERE type = 'nextcloud_talk';
ALTER TABLE channels DROP CONSTRAINT channels_type_check;
ALTER TABLE channels ADD CONSTRAINT channels_type_check CHECK (type IN ('telegram', 'email', 'webhook', 'script'));

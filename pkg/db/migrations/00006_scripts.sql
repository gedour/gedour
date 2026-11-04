-- JS scripts and the script channel type that runs them.
-- +goose Up
CREATE TABLE scripts (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id   uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name       text NOT NULL CHECK (name <> ''),
    source     text NOT NULL CHECK (octet_length(source) <= 65536),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (owner_id, name)
);

ALTER TABLE channels DROP CONSTRAINT channels_type_check;
ALTER TABLE channels ADD CONSTRAINT channels_type_check CHECK (type IN ('telegram', 'email', 'webhook', 'script'));
-- A script can go only once no channel runs it: the API looks them up by config.
CREATE INDEX channels_script_idx ON channels ((config ->> 'script_id')) WHERE type = 'script';

-- Script channels: the log lines of the last run, capped by the sandbox.
ALTER TABLE deliveries ADD COLUMN logs text;

-- +goose Down
ALTER TABLE deliveries DROP COLUMN logs;
DROP INDEX channels_script_idx;
DELETE FROM channels WHERE type = 'script';
ALTER TABLE channels DROP CONSTRAINT channels_type_check;
ALTER TABLE channels ADD CONSTRAINT channels_type_check CHECK (type IN ('telegram', 'email', 'webhook'));
DROP TABLE scripts;

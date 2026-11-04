-- Channels, routing rules, per-user inbox and channel deliveries.
-- +goose Up
CREATE TABLE channels (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id          uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name              text NOT NULL CHECK (name <> ''),
    -- Named, so a new type is one DROP + ADD CONSTRAINT statement.
    type              text NOT NULL CONSTRAINT channels_type_check CHECK (type IN ('telegram', 'email', 'webhook')),
    -- Non-secret settings (chat id, recipients, URL); the secret lives sealed below.
    config            jsonb NOT NULL DEFAULT '{}',
    -- AES-256-GCM under GEDOUR_ENCRYPTION_KEY, AAD = id, as credentials.
    secret_ciphertext bytea,
    secret_nonce      bytea,
    key_version       smallint,
    last4             text NOT NULL DEFAULT '',
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (owner_id, name)
);

CREATE TABLE rules (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id   uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name       text NOT NULL CHECK (name <> ''),
    -- NULL: every watch the owner can read, owned or shared.
    watch_id   uuid REFERENCES watches (id) ON DELETE CASCADE,
    filter     text NOT NULL DEFAULT '',
    enabled    boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (owner_id, name)
);
CREATE INDEX rules_owner_id_idx ON rules (owner_id);
CREATE INDEX rules_watch_id_idx ON rules (watch_id);

CREATE TABLE rule_channels (
    rule_id    uuid NOT NULL REFERENCES rules (id) ON DELETE CASCADE,
    -- RESTRICT: the API names the rules to edit before a channel can go.
    channel_id uuid NOT NULL REFERENCES channels (id) ON DELETE RESTRICT,
    PRIMARY KEY (rule_id, channel_id)
);
CREATE INDEX rule_channels_channel_id_idx ON rule_channels (channel_id);

ALTER TABLE events ADD COLUMN routed_at timestamptz;
-- Events stored before routing existed are not replayed into inboxes and channels.
UPDATE events SET routed_at = now();
CREATE INDEX events_unrouted_idx ON events (id) WHERE routed_at IS NULL;

CREATE TABLE inbox_items (
    id          bigserial PRIMARY KEY,
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    event_id    bigint NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    created_at  timestamptz NOT NULL DEFAULT now(),
    read_at     timestamptz,
    archived_at timestamptz,
    -- Several matching rules of one user still make one item.
    UNIQUE (user_id, event_id)
);
CREATE INDEX inbox_items_user_idx ON inbox_items (user_id, archived_at, id DESC);
CREATE INDEX inbox_items_event_id_idx ON inbox_items (event_id);

CREATE TABLE deliveries (
    id              bigserial PRIMARY KEY,
    event_id        bigint NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    rule_id         uuid REFERENCES rules (id) ON DELETE SET NULL,
    channel_id      uuid NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    -- failed: the last attempt failed and a retry is scheduled; dead: no retry left.
    status          text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sent', 'failed', 'dead')),
    attempts        integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    claim_token     uuid,
    leased_until    timestamptz,
    last_error      text,
    sent_at         timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (event_id, channel_id)
);
-- The delivery loop scans due deliveries that still have an attempt to make.
CREATE INDEX deliveries_due_idx ON deliveries (next_attempt_at) WHERE status IN ('pending', 'failed');
CREATE INDEX deliveries_channel_idx ON deliveries (channel_id, id DESC);

-- +goose Down
DROP TABLE deliveries;
DROP TABLE inbox_items;
DROP INDEX events_unrouted_idx;
ALTER TABLE events DROP COLUMN routed_at;
DROP TABLE rule_channels;
DROP TABLE rules;
DROP TABLE channels;

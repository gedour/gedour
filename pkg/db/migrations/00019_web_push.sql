-- Web Push devices and the web_push channel type (010).
-- +goose Up
CREATE TABLE push_devices (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name          text NOT NULL CHECK (name <> ''),
    -- sha256 of the endpoint: one browser subscription is one device, whoever registered it last.
    endpoint_hash bytea NOT NULL UNIQUE,
    -- Endpoint, p256dh and auth sealed together under GEDOUR_ENCRYPTION_KEY, AAD = id: the endpoint is a bearer capability.
    sealed        bytea NOT NULL,
    nonce         bytea NOT NULL,
    key_version   smallint NOT NULL,
    user_agent    text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_push_at  timestamptz
);
CREATE INDEX push_devices_owner_idx ON push_devices (owner_id);

-- The devices a web_push channel picked; a channel with config devices "all" has none.
CREATE TABLE channel_push_devices (
    channel_id uuid NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    device_id  uuid NOT NULL REFERENCES push_devices (id) ON DELETE CASCADE,
    PRIMARY KEY (channel_id, device_id)
);
CREATE INDEX channel_push_devices_device_idx ON channel_push_devices (device_id);

-- Devices that accepted a delivery: a retry skips them.
CREATE TABLE delivery_push_sent (
    delivery_id bigint NOT NULL REFERENCES deliveries (id) ON DELETE CASCADE,
    device_id   uuid NOT NULL REFERENCES push_devices (id) ON DELETE CASCADE,
    PRIMARY KEY (delivery_id, device_id)
);
CREATE INDEX delivery_push_sent_device_idx ON delivery_push_sent (device_id);

ALTER TABLE channels DROP CONSTRAINT channels_type_check;
ALTER TABLE channels ADD CONSTRAINT channels_type_check CHECK (type IN ('telegram', 'nextcloud_talk', 'email', 'webhook', 'script', 'web_push'));

-- +goose Down
DELETE FROM rule_channels WHERE channel_id IN (SELECT id FROM channels WHERE type = 'web_push');
DELETE FROM channels WHERE type = 'web_push';
ALTER TABLE channels DROP CONSTRAINT channels_type_check;
ALTER TABLE channels ADD CONSTRAINT channels_type_check CHECK (type IN ('telegram', 'nextcloud_talk', 'email', 'webhook', 'script'));
DROP TABLE delivery_push_sent;
DROP TABLE channel_push_devices;
DROP TABLE push_devices;

-- The server's Web Push VAPID key pair (010): one row, the private scalar sealed like channel secrets.
-- +goose Up
CREATE TABLE vapid_key (
    -- Always true: the primary key makes a second row impossible, so concurrent first starts keep one key.
    id                 boolean PRIMARY KEY DEFAULT true CHECK (id),
    public_key         bytea NOT NULL,
    private_ciphertext bytea NOT NULL,
    private_nonce      bytea NOT NULL,
    key_version        smallint NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE vapid_key;

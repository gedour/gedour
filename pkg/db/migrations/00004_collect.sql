-- Scheduling columns, collected states, events and the conditional-request cache.
-- +goose Up
ALTER TABLE watches
    ADD COLUMN next_run_at          timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN last_run_at          timestamptz,
    ADD COLUMN last_status          text CHECK (last_status IN ('ok', 'error', 'rate_limited')),
    ADD COLUMN last_error           text,
    ADD COLUMN consecutive_failures integer NOT NULL DEFAULT 0,
    -- The worker's lease, apart from next_run_at so a run-now or an edit never steals a running claim.
    ADD COLUMN claim_token          uuid,
    ADD COLUMN leased_until         timestamptz;
-- The worker's claim scans due, unpaused watches only.
CREATE INDEX watches_due_idx ON watches (next_run_at) WHERE NOT paused;

CREATE TABLE watch_states (
    id          bigserial PRIMARY KEY,
    watch_id    uuid NOT NULL REFERENCES watches (id) ON DELETE CASCADE,
    observed_at timestamptz NOT NULL DEFAULT now(),
    state       jsonb NOT NULL,
    state_hash  text NOT NULL
);
CREATE INDEX watch_states_watch_id_idx ON watch_states (watch_id, id DESC);

CREATE TABLE events (
    id          bigserial PRIMARY KEY,
    watch_id    uuid NOT NULL REFERENCES watches (id) ON DELETE CASCADE,
    type        text NOT NULL,
    occurred_at timestamptz,
    observed_at timestamptz NOT NULL DEFAULT now(),
    payload     jsonb NOT NULL,
    dedup_key   text NOT NULL,
    -- A run replayed after a crash inserts the same keys, so it never duplicates an event (001-AC9).
    UNIQUE (watch_id, dedup_key)
);
CREATE INDEX events_watch_id_idx ON events (watch_id, id DESC);

CREATE TABLE http_cache (
    watch_id      uuid NOT NULL REFERENCES watches (id) ON DELETE CASCADE,
    url           text NOT NULL,
    etag          text NOT NULL DEFAULT '',
    last_modified text NOT NULL DEFAULT '',
    PRIMARY KEY (watch_id, url)
);

-- +goose Down
DROP TABLE http_cache;
DROP TABLE events;
DROP TABLE watch_states;
DROP INDEX watches_due_idx;
ALTER TABLE watches
    DROP COLUMN leased_until,
    DROP COLUMN claim_token,
    DROP COLUMN consecutive_failures,
    DROP COLUMN last_error,
    DROP COLUMN last_status,
    DROP COLUMN last_run_at,
    DROP COLUMN next_run_at;

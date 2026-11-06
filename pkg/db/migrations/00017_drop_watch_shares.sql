-- Watch sharing goes: former sharees lose their pinned rules, inbox items and deliveries on the shared watch, each share leaves a system audit row, then the table drops.
-- +goose Up
-- First: an old pod's share-aware query waits here and fails on the drop, instead of writing a sharee row after the deletes.
LOCK TABLE watch_shares IN ACCESS EXCLUSIVE MODE;
-- Through the share rows, so a rule left by a transfer stays (009-AC3).
DELETE FROM rules r USING watch_shares s WHERE s.watch_id = r.watch_id AND s.user_id = r.owner_id;
DELETE FROM deliveries d USING channels c, events e, watch_shares s
    WHERE c.id = d.channel_id AND e.id = d.event_id AND s.watch_id = e.watch_id AND s.user_id = c.owner_id;
DELETE FROM inbox_items i USING events e, watch_shares s
    WHERE e.id = i.event_id AND s.watch_id = e.watch_id AND s.user_id = i.user_id;
-- target_id and before as pkg/watch wrote them for a removed share: the audit view and a manual restore read them back.
INSERT INTO audit_log (action, target_type, target_id, before)
SELECT 'watch_share.removed', 'watch_share', s.watch_id::text || ':' || s.user_id::text,
       jsonb_build_object('user_id', s.user_id, 'email', u.email, 'display_name', u.display_name,
                          'permission', s.permission, 'created_at', s.created_at)
    FROM watch_shares s JOIN users u ON u.id = s.user_id;
-- Last, after the row locks above: removing the FK triggers takes ACCESS EXCLUSIVE on watches and users, bounded by lock_timeout; the pod restart retries.
DROP TABLE watch_shares;
DROP FUNCTION watch_shares_not_owner();

-- +goose Down
-- Empty: the deleted rules, inbox items and deliveries come back only from a backup.
CREATE TABLE watch_shares (
    watch_id   uuid NOT NULL REFERENCES watches (id) ON DELETE CASCADE,
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    permission text NOT NULL CHECK (permission IN ('ro', 'rw')),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (watch_id, user_id)
);
CREATE INDEX watch_shares_user_id_idx ON watch_shares (user_id);

-- +goose StatementBegin
CREATE FUNCTION watch_shares_not_owner() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM watches WHERE id = NEW.watch_id AND owner_id = NEW.user_id) THEN
        RAISE EXCEPTION 'a watch cannot be shared with its owner' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER watch_shares_not_owner BEFORE INSERT OR UPDATE ON watch_shares
    FOR EACH ROW EXECUTE FUNCTION watch_shares_not_owner();

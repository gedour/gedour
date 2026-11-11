-- A rule covers all its owner's watches or a list of them (014): the list moves to rule_watches.
-- +goose Up
CREATE TABLE rule_watches (
    rule_id  uuid NOT NULL REFERENCES rules (id) ON DELETE CASCADE,
    watch_id uuid NOT NULL REFERENCES watches (id) ON DELETE CASCADE,
    PRIMARY KEY (rule_id, watch_id)
);
CREATE INDEX rule_watches_watch_id_idx ON rule_watches (watch_id);
-- false with no rule_watches row routes nothing.
ALTER TABLE rules ADD COLUMN all_watches boolean NOT NULL DEFAULT true;
ALTER TABLE rules ALTER COLUMN all_watches DROP DEFAULT;
UPDATE rules SET all_watches = false WHERE watch_id IS NOT NULL;
INSERT INTO rule_watches (rule_id, watch_id) SELECT id, watch_id FROM rules WHERE watch_id IS NOT NULL;
DROP INDEX rules_watch_id_idx;
ALTER TABLE rules DROP COLUMN watch_id;

-- +goose Down
-- +goose StatementBegin
DO $$
DECLARE
    bad text;
BEGIN
    SELECT string_agg(format('%s (%s)', r.id, r.name), ', ' ORDER BY r.id) INTO bad
    FROM rules r
    WHERE (SELECT count(*) FROM rule_watches rw WHERE rw.rule_id = r.id) <> CASE WHEN r.all_watches THEN 0 ELSE 1 END;
    IF bad IS NOT NULL THEN
        RAISE EXCEPTION 'rules on several watches or none, edit them to one watch or all watches first: %', bad;
    END IF;
    -- In the block: run by hand, psql would go on past the refusal to the statements after it.
    ALTER TABLE rules ADD COLUMN watch_id uuid REFERENCES watches (id) ON DELETE CASCADE;
    UPDATE rules r SET watch_id = rw.watch_id FROM rule_watches rw WHERE rw.rule_id = r.id;
    CREATE INDEX rules_watch_id_idx ON rules (watch_id);
    ALTER TABLE rules DROP COLUMN all_watches;
    DROP TABLE rule_watches;
END $$;
-- +goose StatementEnd

-- Each digest time runs on some weekdays (018): one mask per digest_at entry, bit 0 Monday to bit 6 Sunday.
-- +goose Up
ALTER TABLE rules ADD COLUMN digest_days smallint[];
UPDATE rules SET digest_days = array_fill(127::smallint, ARRAY[cardinality(digest_at)])
    WHERE delivery = 'digest' AND digest_at IS NOT NULL;
ALTER TABLE rules ADD CONSTRAINT rules_digest_days_check
    CHECK (cardinality(digest_days) = cardinality(digest_at) AND 0 < ALL (digest_days) AND 128 > ALL (digest_days)
        -- ALL is NULL, not false, on a NULL element.
        AND array_position(digest_days, NULL) IS NULL);

-- +goose Down
-- +goose StatementBegin
DO $$
DECLARE
    bad text;
BEGIN
    SELECT string_agg(format('%s (%s)', id, name), ', ' ORDER BY id) INTO bad
    FROM rules WHERE 127 <> ANY (digest_days);
    IF bad IS NOT NULL THEN
        RAISE EXCEPTION 'digest rules not on every day, edit them to every day first: %', bad;
    END IF;
    ALTER TABLE rules DROP COLUMN digest_days;
END $$;
-- +goose StatementEnd

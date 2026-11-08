-- Digest moves from the channel to the rule (012 phase 4): a rule takes the schedule its digest channels share.
-- +goose Up
ALTER TABLE rules ADD COLUMN delivery text NOT NULL DEFAULT 'immediate' CHECK (delivery IN ('immediate', 'digest')),
    ADD COLUMN digest_at text[],
    ADD COLUMN digest_tz text;

-- +goose StatementBegin
DO $$
DECLARE
    r record;
BEGIN
    -- A high priority rule sent at once on digest channels already: it stays immediate, unlogged.
    FOR r IN
        -- ok: a usable schedule; a digest channel without one (at not a non-empty array) makes the rule ambiguous.
        SELECT rule_id,
               count(DISTINCT (at, tz)) FILTER (WHERE digest AND ok) AS schedules,
               count(*) FILTER (WHERE digest AND NOT ok) AS malformed,
               count(*) FILTER (WHERE NOT digest AND batch) AS immediates,
               min(at::text) FILTER (WHERE digest AND ok) AS at, min(tz) FILTER (WHERE digest AND ok) AS tz
        FROM (
            SELECT rc.rule_id, c.config->'at' AS at, COALESCE(c.config->>'tz', 'UTC') AS tz,
                   c.config->>'delivery' IS NOT DISTINCT FROM 'digest' AS digest,
                   c.type IN ('telegram', 'nextcloud_talk', 'email', 'web_push') AS batch,
                   CASE WHEN jsonb_typeof(c.config->'at') = 'array' THEN jsonb_array_length(c.config->'at') > 0 ELSE false END AS ok
            FROM rule_channels rc JOIN channels c ON c.id = rc.channel_id JOIN rules ru ON ru.id = rc.rule_id
            WHERE ru.priority <> 'high'
        ) x
        GROUP BY rule_id
        HAVING bool_or(digest)
    LOOP
        -- An immediate channel that can list events would start waiting under a digest rule: ambiguous too.
        IF r.schedules = 1 AND r.malformed = 0 AND r.immediates = 0 THEN
            UPDATE rules SET delivery = 'digest', digest_at = ARRAY(SELECT jsonb_array_elements_text(r.at::jsonb)), digest_tz = r.tz
                WHERE id = r.rule_id;
        ELSE
            -- Forwarded to the app log by db.Migrate (migration_warning).
            RAISE WARNING 'rule % left immediate: its channels do not share one digest schedule', r.rule_id;
        END IF;
    END LOOP;
END $$;
-- +goose StatementEnd

UPDATE channels SET config = config - 'delivery' - 'at' - 'tz' WHERE config ?| ARRAY['delivery', 'at', 'tz'];

-- +goose Down
-- A channel gets a schedule back only when all its normal priority rules are digests of that one schedule:
-- a channel digest would hold the events of a rule that sends at once.
UPDATE channels c SET config = c.config || jsonb_build_object('delivery', 'digest', 'at', to_jsonb(s.at), 'tz', s.tz)
FROM (
    SELECT rc.channel_id, min(r.digest_at::text)::text[] AS at, min(r.digest_tz) AS tz
    FROM rule_channels rc JOIN rules r ON r.id = rc.rule_id
    WHERE r.priority <> 'high'
    GROUP BY rc.channel_id
    HAVING bool_and(r.delivery = 'digest') AND count(DISTINCT (r.digest_at, r.digest_tz)) = 1
) s
WHERE c.id = s.channel_id AND c.type IN ('telegram', 'nextcloud_talk', 'email', 'web_push');
ALTER TABLE rules DROP COLUMN delivery, DROP COLUMN digest_at, DROP COLUMN digest_tz;

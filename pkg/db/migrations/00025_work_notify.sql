-- Writes to the scheduling data wake the worker loops (015): one NOTIFY on gedour_work, the table name as payload.
-- Statement level: a bulk statement sends one notification. NOTIFY is delivered at commit, never on a rollback.
-- CREATE TRIGGER takes a SHARE ROW EXCLUSIVE lock, brief and bounded by lock_timeout (migrationLockTimeout).
-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION gedour_work_notify() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('gedour_work', TG_TABLE_NAME);
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
-- Only the columns the claim queries schedule on: a lease write must not wake every replica.
CREATE TRIGGER watches_work_notify AFTER INSERT OR UPDATE OF next_run_at, paused ON watches
    FOR EACH STATEMENT EXECUTE FUNCTION gedour_work_notify();
CREATE TRIGGER events_work_notify AFTER INSERT ON events
    FOR EACH STATEMENT EXECUTE FUNCTION gedour_work_notify();
CREATE TRIGGER deliveries_work_notify AFTER INSERT OR UPDATE OF next_attempt_at, status ON deliveries
    FOR EACH STATEMENT EXECUTE FUNCTION gedour_work_notify();

-- +goose Down
DROP TRIGGER deliveries_work_notify ON deliveries;
DROP TRIGGER events_work_notify ON events;
DROP TRIGGER watches_work_notify ON watches;
DROP FUNCTION gedour_work_notify();

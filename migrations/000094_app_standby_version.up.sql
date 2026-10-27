-- One writer per app's standby (R-153; notes-cluster.md "As built: third
-- review fixes").
--
-- A standby row is written compare-and-set on the row as it was read
-- (state.Standbys.SaveIf). Comparing a list of columns missed the ones it
-- left out: a write from a reading taken before a returning host's stale
-- copy was removed put stale_host back, and the copy was removed twice. The
-- row now carries a version that every UPDATE changes, whoever makes it —
-- the standby pass, a promotion, a host's removal from the cluster — and a
-- conditional write compares that alone.
ALTER TABLE app_standbys ADD COLUMN version bigint NOT NULL DEFAULT 0;

CREATE FUNCTION app_standbys_next_version() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    NEW.version := OLD.version + 1;
    RETURN NEW;
END;
$$;

CREATE TRIGGER app_standbys_version BEFORE UPDATE ON app_standbys
    FOR EACH ROW EXECUTE FUNCTION app_standbys_next_version();

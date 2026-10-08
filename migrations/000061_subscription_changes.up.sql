-- Tells every replica that what event routing reads about subscriptions has
-- changed (issue #72, docs/design/notes-background-costs-issue-72.md).
--
-- The event dispatcher keeps the enabled subscriptions in memory on every
-- replica, and used to read them again — three joins over every subscription
-- — every few seconds whether or not anything had changed. A NOTIFY on the
-- channel below, delivered when a transaction that changed one commits, lets
-- each replica keep its copy until it is told otherwise.
--
-- A trigger rather than a call in the store, so a subscription removed by a
-- cascade, or changed by a path written later, is told too. Only the columns
-- routing reads count: a delivery's success or failure updates the row on
-- every send, and is not a reason for every replica to read the list again.
-- Postgres folds identical notifications in one transaction into one.
CREATE FUNCTION subscriptions_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('pando_subscriptions', '');
    RETURN NULL;
END $$;

CREATE TRIGGER subscriptions_changed_insert_delete
    AFTER INSERT OR DELETE ON subscriptions
    FOR EACH ROW EXECUTE FUNCTION subscriptions_changed();

CREATE TRIGGER subscriptions_changed_update
    AFTER UPDATE ON subscriptions
    FOR EACH ROW WHEN (OLD.enabled IS DISTINCT FROM NEW.enabled
                       OR OLD.events IS DISTINCT FROM NEW.events
                       OR OLD.app_id IS DISTINCT FROM NEW.app_id
                       OR OLD.created_at IS DISTINCT FROM NEW.created_at)
    EXECUTE FUNCTION subscriptions_changed();

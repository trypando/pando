-- Event subscriptions: an outbox of events, subscriptions to them, and a record
-- of every delivery (issue #50, R-364 – R-374, design 11).
--
-- Events come from three places, and all three land in one table:
--
--   1. The audit log. A catalogued audit action (app.deploy, grant.create, …)
--      is copied into `events` by the audit writer, in the same transaction as
--      the audit row, so one never exists without the other.
--   2. Triggers, below, for state that changes without an audited action: an
--      app's state, a deploy's outcome, a backup taken or failed on schedule.
--      A trigger is the one place every writer of those columns passes through.
--   3. Core, directly, for the two events that are neither: adapter health,
--      and a test delivery somebody asked for.
--
-- Delivery reads only this table, so an event recorded is an event delivered
-- at least once, across restarts (R-366).

-- A ULID, as internal/id makes one: 48 bits of milliseconds and 80 random bits
-- in Crockford base32. Triggers need IDs in the same shape as Go's, so an event
-- written here and one written by core sort and read the same.
CREATE FUNCTION pando_ulid() RETURNS text
LANGUAGE plpgsql VOLATILE AS $$
DECLARE
    alphabet text := '0123456789ABCDEFGHJKMNPQRSTVWXYZ';
    ms       bigint := floor(extract(epoch FROM clock_timestamp()) * 1000);
    -- A v4 UUID's version and variant bits are fixed (bytes 6 and 8), so the
    -- random part is taken from the bytes around them.
    raw      bytea := uuid_send(gen_random_uuid());
    rnd      bytea := substring(raw FROM 1 FOR 6) || substring(raw FROM 10 FOR 4);
    out      text := '';
    acc      bigint := 0;
    bits     int := 0;
BEGIN
    FOR i IN REVERSE 9..0 LOOP
        out := out || substr(alphabet, ((ms >> (i * 5)) & 31)::int + 1, 1);
    END LOOP;
    FOR i IN 0..9 LOOP
        acc := (acc << 8) | get_byte(rnd, i);
        bits := bits + 8;
        WHILE bits >= 5 LOOP
            bits := bits - 5;
            out := out || substr(alphabet, ((acc >> bits) & 31)::int + 1, 1);
        END LOOP;
        acc := acc & ((1::bigint << bits) - 1);
    END LOOP;
    RETURN out;
END $$;

CREATE TABLE events (
    id           text PRIMARY KEY DEFAULT ('evt_' || pando_ulid()),

    -- Insertion order, for routing in the order things happened.
    seq          bigint GENERATED ALWAYS AS IDENTITY UNIQUE,

    -- A catalogued name (internal/core/events). Not a CHECK against the
    -- catalog: the catalog grows by code, and an old binary reading a newer
    -- name is told by the catalog, not by an insert that fails.
    name         text NOT NULL CHECK (name ~ '^[a-z][a-z_]*(\.[a-z][a-z_]*)+$'),

    -- NULL for install-wide events. Not a foreign key: the event about an
    -- app's deletion outlives the app, and so does its history.
    app_id       text,

    actor_kind   text NOT NULL DEFAULT 'system'
                 CHECK (actor_kind IN ('user', 'token', 'system', 'anonymous')),
    actor_id     text,
    on_behalf_of text,
    request_id   text,

    -- The event's fields, as the catalog lists them. Never a secret value
    -- (R-194): the audit detail this is copied from never holds one either.
    data         jsonb NOT NULL DEFAULT '{}'::jsonb,

    occurred_at  timestamptz NOT NULL DEFAULT now(),

    -- Set once every matching subscription has a delivery row.
    routed_at    timestamptz
);

CREATE INDEX events_unrouted_idx ON events (seq) WHERE routed_at IS NULL;
CREATE INDEX events_occurred_idx ON events (occurred_at);
CREATE INDEX events_app_idx ON events (app_id, occurred_at DESC) WHERE app_id IS NOT NULL;

-- app.state_changed: every writer of apps.state passes through here, which is
-- why this is a trigger and not a call somebody has to remember. Health is
-- part of state (running ↔ degraded, design 05 §1), so this is also how a
-- health change is told.
CREATE FUNCTION events_app_state() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO events (name, app_id, data)
    VALUES ('app.state_changed', NEW.id,
            jsonb_build_object('from', OLD.state, 'to', NEW.state));
    RETURN NULL;
END $$;

CREATE TRIGGER apps_state_event
    AFTER UPDATE OF state ON apps
    FOR EACH ROW WHEN (OLD.state IS DISTINCT FROM NEW.state)
    EXECUTE FUNCTION events_app_state();

-- deploy.succeeded / deploy.failed: the deploy runner finishes a deployment by
-- writing its status and nothing else, so the outcome is told from the row.
CREATE FUNCTION events_deploy_outcome() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO events (name, app_id, data)
    VALUES ('deploy.' || NEW.status, NEW.app_id,
            jsonb_strip_nulls(jsonb_build_object(
                'deployment_id', NEW.id,
                'spec_id', NEW.spec_id,
                'trigger', NEW.trigger,
                'error_code', NEW.error_code,
                'message', NEW.error_detail->>'message')));
    RETURN NULL;
END $$;

CREATE TRIGGER deployments_outcome_event
    AFTER UPDATE OF status ON deployments
    FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status
                       AND NEW.status IN ('succeeded', 'failed'))
    EXECUTE FUNCTION events_deploy_outcome();

-- backup.created: a row here is a backup that exists, however it was taken.
CREATE FUNCTION events_backup_created() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO events (name, app_id, data)
    VALUES ('backup.created', NEW.app_id,
            jsonb_strip_nulls(jsonb_build_object(
                'backup_id', NEW.id, 'kind', NEW.kind, 'size_bytes', NEW.size_bytes)));
    RETURN NULL;
END $$;

CREATE TRIGGER backups_created_event
    AFTER INSERT ON backups
    FOR EACH ROW EXECUTE FUNCTION events_backup_created();

-- backup.failed, for the scheduled backup that nobody asked for and so nobody
-- is waiting on (issue #87). A failure somebody asked for is audited and
-- reaches the outbox that way.
CREATE FUNCTION events_backup_attempt() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO events (name, app_id, data)
    VALUES ('backup.failed', NEW.app_id,
            jsonb_build_object('scheduled', true, 'message', NEW.message, 'remedy', NEW.remedy));
    RETURN NULL;
END $$;

CREATE TRIGGER backup_attempts_failed_event
    AFTER INSERT OR UPDATE ON backup_attempts
    FOR EACH ROW WHEN (NEW.outcome = 'failed')
    EXECUTE FUNCTION events_backup_attempt();

-- A subscription: which events, about what, sent where (R-367).
CREATE TABLE subscriptions (
    id            text PRIMARY KEY,          -- sub_...

    -- A person. Deliveries are authorized as them, every time (R-368), so a
    -- subscription is worth no more than its owner's live grants.
    owner_id      text NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    -- NULL subscribes install-wide, which needs install.events.manage.
    app_id        text REFERENCES apps(id) ON DELETE CASCADE,

    -- Names and wildcards: deploy.failed, deploy.*, *.
    events        text[] NOT NULL CHECK (cardinality(events) > 0),

    destination   text NOT NULL CHECK (destination IN ('webhook', 'notify')),
    url           text,
    adapter_id    text,

    description   text NOT NULL DEFAULT '',

    enabled         boolean NOT NULL DEFAULT true,
    disabled_reason text NOT NULL DEFAULT '',

    -- When an endpoint started failing every attempt, and how many in a row.
    -- An endpoint that has failed everything for a day is turned off (R-370).
    failing_since        timestamptz,
    consecutive_failures integer NOT NULL DEFAULT 0,

    created_by    text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT subscriptions_webhook_has_url CHECK ((destination = 'webhook') = (url IS NOT NULL)),
    CONSTRAINT subscriptions_notify_has_adapter CHECK ((destination = 'notify') = (adapter_id IS NOT NULL))
);

CREATE INDEX subscriptions_owner_idx ON subscriptions (owner_id);
CREATE INDEX subscriptions_app_idx ON subscriptions (app_id);

-- A webhook's signing key, sealed by the secrets adapter (R-371, R-190). The
-- subscription row has no column a key could be put in, and this one holds
-- ciphertext or an external reference, never the key.
CREATE TABLE subscription_signing_keys (
    subscription_id text PRIMARY KEY REFERENCES subscriptions(id) ON DELETE CASCADE,
    adapter_ref     text NOT NULL,
    ciphertext      bytea,
    external_ref    text,
    version         integer NOT NULL DEFAULT 1,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CHECK (ciphertext IS NOT NULL OR external_ref IS NOT NULL)
);

-- One event to one subscription.
CREATE TABLE event_deliveries (
    id               text PRIMARY KEY,       -- dlv_...
    subscription_id  text NOT NULL REFERENCES subscriptions(id) ON DELETE CASCADE,
    event_id         text NOT NULL REFERENCES events(id) ON DELETE CASCADE,

    status           text NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending', 'succeeded', 'failed')),
    attempts         integer NOT NULL DEFAULT 0,
    next_attempt_at  timestamptz,
    last_attempt_at  timestamptz,
    last_status_code integer,
    last_error       text NOT NULL DEFAULT '',

    -- Who asked for it to be sent again, when somebody did, and how many
    -- attempts had been made by then: a redelivery gets the whole retry
    -- schedule again, counted from here.
    redelivered_by   text,
    round_base       integer NOT NULL DEFAULT 0,

    created_at       timestamptz NOT NULL DEFAULT now(),

    UNIQUE (subscription_id, event_id)
);

CREATE INDEX event_deliveries_due_idx ON event_deliveries (next_attempt_at) WHERE status = 'pending';
CREATE INDEX event_deliveries_subscription_idx ON event_deliveries (subscription_id, created_at DESC);

-- Every attempt at a delivery, kept with it (R-369).
CREATE TABLE delivery_attempts (
    delivery_id  text NOT NULL REFERENCES event_deliveries(id) ON DELETE CASCADE,
    attempt      integer NOT NULL,
    attempted_at timestamptz NOT NULL,
    status_code  integer,
    error        text NOT NULL DEFAULT '',
    duration_ms  integer NOT NULL,
    PRIMARY KEY (delivery_id, attempt)
);

-- Which of Pando's own notifications reach a person, on which channel
-- (R-373). A missing row is the default; a row is a choice the person made.
CREATE TABLE notification_preferences (
    user_id    text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind       text NOT NULL,
    channel    text NOT NULL,                -- a notify adapter's ID
    enabled    boolean NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, kind, channel)
);

-- install.events.manage: install-wide subscriptions, and everybody's. The
-- Administrator's, and in no other built-in role. Built-in roles change only by
-- migration (R-081).
ALTER TABLE roles DISABLE TRIGGER roles_builtin_immutable;

UPDATE roles
SET verbs = verbs || 'install.events.manage'::text
WHERE id = 'role_administrator';

ALTER TABLE roles ENABLE TRIGGER roles_builtin_immutable;

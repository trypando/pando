-- Warm standby (R-419, R-420, design notes-cluster.md).
--
-- One row per app that has, or had, a standby: where the runtime keeps it and
-- how current its copy is. The host is the runtime's own name for the place
-- (api.StandbyHandle), recorded to show and to hand back; core never chooses
-- it (R-256). A row outlives its app's soft delete until the standby pass has
-- removed the standby from the runtime, then it is deleted.
CREATE TABLE app_standbys (
    app_id         text PRIMARY KEY REFERENCES apps(id) ON DELETE CASCADE,
    -- The runtime adapter keeping it, to remove it through once the app is
    -- gone and its spec no longer says.
    runtime_ref    text NOT NULL DEFAULT '',
    -- Where the standby is. Empty until the runtime has made one.
    host           text NOT NULL DEFAULT '',
    -- The pinned spec the standby was made from; a different pinned spec
    -- means it is made again.
    spec_id        text,
    -- How many app volumes are shipped, and how many copies the standby
    -- holds of the volume with the fewest (R-420 [P]: three are kept).
    volumes        integer NOT NULL DEFAULT 0 CHECK (volumes >= 0),
    copies         integer NOT NULL DEFAULT 0 CHECK (copies >= 0),
    shipped_at     timestamptz,
    -- Provisioned services whose kind does not replicate natively, so their
    -- data is in the volume copy and may be inconsistent there (R-420).
    unreplicated   text[] NOT NULL DEFAULT '{}',
    -- The last thing that went wrong keeping it current, R-105 text.
    last_error     text NOT NULL DEFAULT '',
    -- After a failover: where the app ran before, which holds a stale copy
    -- to remove when it returns (R-419), and when.
    stale_host     text NOT NULL DEFAULT '',
    promoted_at    timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX app_standbys_stale ON app_standbys (app_id) WHERE stale_host <> '';

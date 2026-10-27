-- Where each app with a standby runs, and why its standby could not be
-- placed (R-419, R-421, R-422, notes-cluster.md "As built: wiring standby,
-- capacity and fencing").
ALTER TABLE app_standbys
    -- The runtime's name for the place the app runs, as ApplyStandby or
    -- Promote last reported it (api.StandbyHandle.Home). The liveness pass
    -- reads it to know which apps a lost host held; it is the same name as
    -- the host's in cluster_hosts. Empty until the runtime has said.
    ADD COLUMN home_host         text NOT NULL DEFAULT '',
    -- The last error's stable code and remedy, beside last_error's message:
    -- CAPACITY_WOULD_OVERSUBSCRIBE is the app's no-room notice (R-422).
    ADD COLUMN last_error_code   text NOT NULL DEFAULT '',
    ADD COLUMN last_error_remedy text NOT NULL DEFAULT '';

CREATE INDEX app_standbys_home ON app_standbys (home_host) WHERE home_host <> '';
CREATE INDEX app_standbys_stale_host ON app_standbys (stale_host) WHERE stale_host <> '';

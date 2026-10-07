-- A deploy and detection work queue, and the indexes background work and
-- retention need (issue #72, PR 4; O-32).
--
-- A deployment in `pending` with no replica_id is queued: any replica claims
-- it with FOR UPDATE SKIP LOCKED, up to its own concurrency limit, and stamps
-- replica_id and claimed_at. A detection in `running` with no replica_id is
-- queued the same way. The claimant's heartbeat in pando_replicas is the
-- lease: work claimed by a replica that has stopped or gone silent is put back
-- in the queue, up to `attempts`, and recorded as interrupted after that.
ALTER TABLE deployments ADD COLUMN claimed_at timestamptz;
ALTER TABLE deployments ADD COLUMN attempts integer NOT NULL DEFAULT 0;
ALTER TABLE detections ADD COLUMN claimed_at timestamptz;
ALTER TABLE detections ADD COLUMN attempts integer NOT NULL DEFAULT 0;

-- What the queue reads and the sweeper scans every 15 seconds: work in
-- flight, which is a handful of rows among every deployment ever made.
CREATE INDEX deployments_in_flight_idx ON deployments (started_at)
    WHERE status IN ('pending', 'building', 'applying');
CREATE INDEX detections_queued_idx ON detections (started_at)
    WHERE status = 'running';

-- Is this revision deployed anywhere: spec revision pruning, R-157's
-- "ran successfully before", and deployment retention all ask it.
CREATE INDEX deployments_spec_idx ON deployments (spec_id);

-- Pruning an event cascades into its deliveries, which had no index on
-- event_id: only UNIQUE (subscription_id, event_id), which leads with the
-- other column. Each pruned event was a scan of every delivery.
CREATE INDEX event_deliveries_event_idx ON event_deliveries (event_id);

-- Retention of sessions, which nothing removed: an expired or revoked session
-- is a row nobody can use.
CREATE INDEX sessions_expiry_idx ON sessions (expires_at);

-- The rolling-backup job finds apps due one by their newest rolling backup.
CREATE INDEX backups_rolling_idx ON backups (app_id, created_at DESC) WHERE kind = 'rolling';

-- The retention job removes spent one-time sign-in identifiers by expiry. It
-- used to happen on every sign-in, in the request.
CREATE INDEX sso_replay_expires_idx ON sso_replay (expires_at);

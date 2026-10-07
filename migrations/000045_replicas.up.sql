-- Several Pando processes against one database (issue #72).
--
-- A replica is one running Pando process — this install's own control plane,
-- never a place an app runs. It is not a host, and nothing that plans or
-- places a workload reads it (R-010, R-256): this table answers "which of
-- Pando's own processes are alive", and nothing else.
--
-- A replica's id is fresh on every start, so a restarted process is a new
-- replica and whatever the old one had under way is recognizably nobody's.
CREATE TABLE pando_replicas (
    id            text PRIMARY KEY CHECK (id ~ '^rep_'),
    hostname      text NOT NULL,
    -- Where the other replicas reach this one, for the one thing that has to
    -- be asked of a particular replica: a deploy's live log (design note
    -- notes-multiple-replicas-issue-72.md). Empty when it cannot be reached.
    advertise_url text NOT NULL DEFAULT '',
    version       text NOT NULL DEFAULT '',
    -- The public half of the key this replica signs identity assertions with
    -- (R-051). Public only: the private key never leaves the process. Every
    -- replica publishes every live replica's key in its JWKS, so an app
    -- verifies an assertion whichever replica signed it.
    assertion_kid text NOT NULL,
    assertion_key bytea NOT NULL CHECK (length(assertion_key) = 32),
    started_at    timestamptz NOT NULL DEFAULT now(),
    heartbeat_at  timestamptz NOT NULL DEFAULT now(),
    stopped_at    timestamptz
);

-- Which replica is running a deploy or a detection. Work under way on a
-- replica that has stopped heartbeating will never finish, and is recorded as
-- interrupted — by whichever replica notices, not by the next one to start,
-- which used to fail every other replica's live deploys.
ALTER TABLE deployments ADD COLUMN replica_id text;
ALTER TABLE detections ADD COLUMN replica_id text;

-- Wrong passcodes, per app and client address, for the limit on guessing one
-- (R-075a). In the database rather than in memory so that N replicas behind a
-- load balancer give an attacker the same number of attempts as one.
CREATE TABLE passcode_failures (
    key       text NOT NULL,
    failed_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX passcode_failures_key ON passcode_failures (key, failed_at);

-- A value sealed with the install's secrets key, which every replica opens at
-- start (R-190). The key is not in the database — that is the point of it —
-- so replicas must each be given the same one, and this is how a replica given
-- a different one finds out before it writes a secret nobody else can read.
-- digest is the SHA-256 of a random value that protects nothing.
CREATE TABLE secrets_canary (
    id          integer PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    adapter_ref text NOT NULL,
    ciphertext  bytea NOT NULL,
    digest      bytea NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- Install-wide signals every replica watches. One row (R-015).
CREATE TABLE cluster_signals (
    id                   integer PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    -- POST /restart (R-253). Every replica started before this restarts, so
    -- the adapter configuration they run cannot drift apart.
    restart_requested_at timestamptz
);
INSERT INTO cluster_signals (id) VALUES (1);

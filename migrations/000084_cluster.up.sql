-- A Pando cluster (R-414, R-415, design notes-cluster.md).
--
-- cluster_hosts is the cluster's membership: every machine running Pando's
-- binary that has joined this install, with the roles it holds. Placement is
-- not here: which host a workload goes on stays the runtime adapter's (R-256).
CREATE TABLE cluster_hosts (
    id           text PRIMARY KEY CHECK (id LIKE 'host\_%'),
    name         text NOT NULL UNIQUE CHECK (length(name) BETWEEN 1 AND 100),
    roles        text[] NOT NULL CHECK (
        cardinality(roles) BETWEEN 1 AND 2
        AND roles <@ ARRAY['control', 'app']::text[]
    ),
    -- Where the host's agent listens, host:port (R-416).
    address      text NOT NULL,
    -- SHA-256 of the host's certificate, hex, so a host is known by the key it
    -- joined with and a replaced machine under the same name is not mistaken
    -- for it.
    cert_sha256  text NOT NULL CHECK (cert_sha256 ~ '^[0-9a-f]{64}$'),
    platform     text NOT NULL DEFAULT '',
    version      text NOT NULL DEFAULT '',
    joined_with  text,
    joined_at    timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz
);

-- A join token (R-415). Only the keyed digest of its secret is kept, made with
-- the install's API token key as API tokens are (R-063), so a dump of this
-- table cannot be used to join.
CREATE TABLE join_tokens (
    id          text PRIMARY KEY CHECK (id LIKE 'jt\_%'),
    hash        text NOT NULL UNIQUE,
    roles       text[] NOT NULL CHECK (
        cardinality(roles) BETWEEN 1 AND 2
        AND roles <@ ARRAY['control', 'app']::text[]
    ),
    max_uses    integer NOT NULL DEFAULT 1 CHECK (max_uses BETWEEN 1 AND 100),
    uses        integer NOT NULL DEFAULT 0 CHECK (uses >= 0 AND uses <= max_uses),
    created_by  text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    revoked_at  timestamptz,
    CHECK (expires_at > created_at)
);

ALTER TABLE cluster_hosts
    ADD CONSTRAINT cluster_hosts_joined_with_fkey
    FOREIGN KEY (joined_with) REFERENCES join_tokens(id) ON DELETE SET NULL;

CREATE INDEX join_tokens_live ON join_tokens (expires_at) WHERE revoked_at IS NULL;

-- install.cluster.manage (R-080): mint and revoke join tokens, remove a host,
-- change a host's roles. The Administrator gets it by the sanctioned path for
-- changing a built-in role (R-081).
ALTER TABLE roles DISABLE TRIGGER roles_builtin_immutable;

UPDATE roles
SET verbs = verbs || 'install.cluster.manage'::text
WHERE id = 'role_administrator';

ALTER TABLE roles ENABLE TRIGGER roles_builtin_immutable;

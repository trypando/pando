-- Signing an MCP client in with OAuth (R-413, design 06 §3.6). An MCP client
-- registers itself (RFC 7591), a person signed in to the console approves it,
-- and it trades a one-time code with the PKCE verifier only it holds for a
-- delegated token (R-058) — R-406's flow, with the client's own redirect
-- address in place of the CLI's loopback port.
--
-- A client is public: it has no secret, so nothing here is a credential.
-- Registering is open, so a client nobody approved within a day is removed by
-- retention; used_at is set the first time someone approves it.
CREATE TABLE oauth_clients (
    id            text PRIMARY KEY CHECK (id LIKE 'oac\_%'),
    name          text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    redirect_uris text[] NOT NULL CHECK (cardinality(redirect_uris) BETWEEN 1 AND 10),
    created_at    timestamptz NOT NULL DEFAULT now(),
    used_at       timestamptz
);

CREATE INDEX oauth_clients_unused ON oauth_clients (created_at) WHERE used_at IS NULL;

-- An approval waiting to be collected, as cli_sign_ins (000078): only the
-- code's SHA-256 is kept, a row is deleted the first time anyone tries its
-- code, and one nobody tried is removed by retention once expired.
CREATE TABLE oauth_codes (
    code_hash      text PRIMARY KEY,
    client_id      text NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
    user_id        text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    approved_by    text NOT NULL,
    -- base64url(SHA-256(verifier)) without padding: always 43 characters (RFC 7636).
    code_challenge text NOT NULL CHECK (code_challenge ~ '^[A-Za-z0-9_-]{43}$'),
    -- The redirect address the code was sent to, which the exchange must name
    -- again (RFC 6749 §4.1.3).
    redirect_uri   text NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    expires_at     timestamptz NOT NULL
);

CREATE INDEX oauth_codes_expires_at ON oauth_codes (expires_at);

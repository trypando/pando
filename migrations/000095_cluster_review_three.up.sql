-- The owner's third review of #347 (notes-cluster.md "As built: third review
-- fixes").

-- SHA-256 of the public key a host joined with, hex (R-415): what tells a
-- joining host trying again, which may replace its own pending join, from
-- another machine that took the same name, which may not while that join is
-- within its lifetime. Empty for hosts recorded before this migration and for
-- the install's own first host.
ALTER TABLE cluster_hosts ADD COLUMN key_sha256 text
    CHECK (key_sha256 IS NULL OR key_sha256 ~ '^[0-9a-f]{64}$');

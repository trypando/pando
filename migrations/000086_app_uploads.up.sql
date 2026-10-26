-- Uploaded sources in the database (R-417, R-262).
--
-- An upload was a file under /var/lib/pando/uploads, which every control host
-- would have to share. Kept here, a deploy on any control host reads what was
-- uploaded on any other, and the DR bundle's database dump carries it (R-212).
--
-- bytea in chunks rather than one bytea value or a large object:
--   - one bytea of up to 256 MiB is held whole in memory on both sides of
--     every write and read, by Pando and by the server;
--   - a large object outlives its row unless something unlinks it (the
--     lo_manage trigger is an extension, vacuumlo a job), and its privileges
--     are separate from the table's, so applyGrants would not cover it.
-- Chunks are ordinary rows: streamed one at a time, removed with their app by
-- the foreign keys, dumped and restored with everything else.
CREATE TABLE app_uploads (
    app_id     text PRIMARY KEY REFERENCES apps(id) ON DELETE CASCADE,
    -- hex SHA-256 of the whole archive, checked on every read.
    sha256     text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    size       bigint NOT NULL CHECK (size >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE app_upload_chunks (
    app_id text NOT NULL REFERENCES app_uploads(app_id) ON DELETE CASCADE,
    seq    integer NOT NULL CHECK (seq >= 0),
    data   bytea NOT NULL,
    PRIMARY KEY (app_id, seq)
);

-- Already compressed: a gzipped tar gains nothing from TOAST compression.
ALTER TABLE app_upload_chunks ALTER COLUMN data SET STORAGE EXTERNAL;

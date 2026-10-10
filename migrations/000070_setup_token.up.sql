-- The setup token (R-046, issue #130).
--
-- A fresh installation is claimed by whoever sets up its administrator in the
-- console, and that person must now present a one-time token Pando printed to
-- its log. One row at most: an installation has one setup, and the token stops
-- existing the moment it is used. Only a digest is kept — SHA-256 of 256 random
-- bits Pando generated, like the passcode unlock token (design 02 §2.1).
CREATE TABLE setup_token (
    id          integer PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    token_hash  text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

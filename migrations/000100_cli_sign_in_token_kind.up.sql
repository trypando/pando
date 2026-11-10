-- `pando token create` mints through the same browser approval as `pando login`
-- (R-406, issue #396): the CLI only holds a token, and a token cannot mint a
-- token, so a signed-in person approves each one. The approval now says which
-- kind it makes — a delegated token acting as the person who approved it
-- (R-058), or a service token that is its own principal (R-060). Every
-- approval made before this was a delegated one.
ALTER TABLE cli_sign_ins
    ADD COLUMN token_kind text NOT NULL DEFAULT 'delegated'
        CHECK (token_kind IN ('delegated', 'account'));

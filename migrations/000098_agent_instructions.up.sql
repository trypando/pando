-- The company's instructions for agents (R-424).
--
-- What administrators want every agent building on this installation to know:
-- their own practices, preferences and rules, sent after Pando's built-in
-- guidance, which lives in the binary so an upgrade can improve it without
-- touching what an administrator wrote. One row at most, like host policy
-- (R-015): one install, one organization, one set of instructions.
--
-- The size limit is the service's (instructions.MaxCompany); the CHECK is a
-- backstop at four times that, so a change to the limit needs no migration.
CREATE TABLE agent_instructions (
    id          integer PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    body        text NOT NULL CHECK (octet_length(body) <= 131072),
    updated_by  text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

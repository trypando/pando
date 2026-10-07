-- A port-mode app's port, as a column beside address_hostname and
-- address_path (migration 35), so the proxy finds the app a request arrived
-- for with an index rather than by reading every pinned spec's JSON on every
-- request (issue #72).
--
-- Written when a revision is pinned (state.Apps.Pin), in the same transaction
-- as the pin, so the column and the pinned spec cannot disagree. Not unique:
-- which app holds a port is port_allocations' to decide (O-15), and this
-- column only says which port a pinned spec claims, as the JSON it replaces
-- did.
ALTER TABLE apps ADD COLUMN address_port integer;

UPDATE apps a
   SET address_port = (r.body->'routing'->>'port')::integer
  FROM spec_revisions r
 WHERE r.id = a.pinned_spec_id
   AND r.body->'routing'->>'mode' = 'port'
   AND r.body->'routing'->>'port' ~ '^[0-9]{1,5}$';

CREATE INDEX apps_address_port ON apps (address_port)
    WHERE deleted_at IS NULL AND address_port IS NOT NULL;

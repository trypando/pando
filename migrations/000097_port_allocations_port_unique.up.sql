-- One app per port, whatever routing adapter it was allocated on (R-165,
-- issue #348). The proxy listens on a port number and resolves a request on
-- it by number alone (apps.address_port), so two adapters each holding the
-- same port gave one listener two apps. The primary key (adapter_ref, port)
-- allowed it; this does not, so two pins racing for one port on different
-- adapters cannot both commit.
--
-- A deleted app's allocations go first: they lapse anyway, the next time a
-- port is allocated (Ports.Allocate), and one must not keep a port from a
-- live app below.
DELETE FROM port_allocations
 WHERE app_id IN (SELECT id FROM apps WHERE deleted_at IS NOT NULL);

-- A port two adapters still hold stays with the app that was given it first.
-- The other loses only the allocation: it is still pinned to the port, which
-- Ports.Allocate and state.Apps.Pin refuse to hand anyone else, and its next
-- pin of it is refused as taken.
DELETE FROM port_allocations later
 USING port_allocations earlier
 WHERE later.port = earlier.port
   AND (later.allocated_at, later.adapter_ref, later.app_id)
     > (earlier.allocated_at, earlier.adapter_ref, earlier.app_id);

CREATE UNIQUE INDEX port_allocations_port_key ON port_allocations (port);

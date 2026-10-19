-- The Docker services adapter took its image overrides as one `images` map,
-- which no form declared: the console and the CLI could not show it, and
-- saving the adapter from either replaced the stored settings whole and
-- dropped it. Each override is now a setting of its own, <type>_image, and
-- this moves a stored map's overrides into them, keeping any setting already
-- there, so the next save keeps them.
--
-- updated_at is left alone: this records what was already configured, and
-- bumping it would mark the adapter as saved since startup.
UPDATE adapter_configs c
   SET config = (c.config - 'images') || COALESCE((
         SELECT jsonb_object_agg(e.key || '_image', to_jsonb(e.value))
           FROM jsonb_each_text(c.config -> 'images') AS e
          WHERE NOT (c.config ? (e.key || '_image'))
       ), '{}'::jsonb)
 WHERE c.category = 'services'
   AND c.kind = 'docker'
   AND jsonb_typeof(c.config -> 'images') = 'object';

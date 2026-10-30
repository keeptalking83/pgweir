-- Gives the agent role what it needs on a fork, and nothing more. Run it on
-- the fork with the connection string printed by "pgweir branch create" (the
-- role that owns the tables). Grants made here exist only on this fork, so the
-- agent still has no privileges on main.
GRANT USAGE ON SCHEMA public, pgweir TO agent;
GRANT SELECT, INSERT, UPDATE, DELETE ON public.orders, public.customers TO agent;
GRANT USAGE ON ALL SEQUENCES IN SCHEMA public TO agent;
GRANT EXECUTE ON FUNCTION pgweir.tag(jsonb) TO agent;

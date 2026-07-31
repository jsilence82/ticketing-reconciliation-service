-- Creates the SELECT-only role the query API must use.
--
-- Guardrail 3 says no endpoint may mutate reconciliation state. This is the
-- enforcement: the API connects as a role that physically cannot write, so the
-- guarantee does not depend on every future handler being careful.
--
-- Not a migration. Roles are cluster-level, need privileges the app role does
-- not have, and are part of provisioning rather than schema. Run once per
-- environment as a superuser:
--
--     psql -d reconciliation -v api_password="$(openssl rand -base64 32)" \
--          -f deploy/readonly-role.sql
--
-- Then point API_DATABASE_URL at it. cmd/server proves the role really is
-- read-only at startup and refuses to serve if it is not.

\set ON_ERROR_STOP on

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'reconciliation_api') THEN
        CREATE ROLE reconciliation_api LOGIN;
    END IF;
END
$$;

ALTER ROLE reconciliation_api PASSWORD :'api_password';

GRANT CONNECT ON DATABASE :"DBNAME" TO reconciliation_api;
GRANT USAGE ON SCHEMA public TO reconciliation_api;

-- SELECT and nothing else. No INSERT, UPDATE, DELETE or TRUNCATE.
GRANT SELECT ON ALL TABLES IN SCHEMA public TO reconciliation_api;

-- Tables created by future migrations must inherit the same restriction,
-- otherwise the next migration silently hands the API write access to a new
-- table. This is the line most easily forgotten.
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT ON TABLES TO reconciliation_api;

-- Belt and braces: even a mistaken GRANT later cannot make the session
-- writable, because every transaction it opens starts read-only.
ALTER ROLE reconciliation_api SET default_transaction_read_only = on;

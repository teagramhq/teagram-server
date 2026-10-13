DO $migration$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_catalog.pg_roles
        WHERE rolname = 'browser_acceptance_probe'
    ) THEN
        CREATE ROLE browser_acceptance_probe;
    END IF;
END;
$migration$;

ALTER ROLE browser_acceptance_probe
    LOGIN
    NOSUPERUSER
    NOCREATEDB
    NOCREATEROLE
    NOINHERIT
    NOREPLICATION
    NOBYPASSRLS;

DO $migration$
DECLARE
    schema_name TEXT;
    granted_role_name TEXT;
    member_role_name TEXT;
BEGIN
    FOR granted_role_name IN
        SELECT DISTINCT granted_role.rolname
        FROM pg_catalog.pg_auth_members AS membership
        JOIN pg_catalog.pg_roles AS granted_role ON granted_role.oid = membership.roleid
        JOIN pg_catalog.pg_roles AS member_role ON member_role.oid = membership.member
        WHERE member_role.rolname = 'browser_acceptance_probe'
    LOOP
        EXECUTE format('REVOKE %I FROM browser_acceptance_probe', granted_role_name);
    END LOOP;

    FOR member_role_name IN
        SELECT DISTINCT member_role.rolname
        FROM pg_catalog.pg_auth_members AS membership
        JOIN pg_catalog.pg_roles AS granted_role ON granted_role.oid = membership.roleid
        JOIN pg_catalog.pg_roles AS member_role ON member_role.oid = membership.member
        WHERE granted_role.rolname = 'browser_acceptance_probe'
    LOOP
        EXECUTE format('REVOKE browser_acceptance_probe FROM %I', member_role_name);
    END LOOP;

    FOR schema_name IN
        SELECT namespace.nspname
        FROM pg_catalog.pg_namespace AS namespace
        WHERE namespace.nspname <> 'information_schema'
          AND namespace.nspname NOT LIKE 'pg_%'
    LOOP
        EXECUTE format('REVOKE ALL PRIVILEGES ON SCHEMA %I FROM browser_acceptance_probe', schema_name);
        EXECUTE format('REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA %I FROM browser_acceptance_probe', schema_name);
        EXECUTE format('REVOKE ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA %I FROM browser_acceptance_probe', schema_name);
        EXECUTE format('REVOKE ALL PRIVILEGES ON ALL FUNCTIONS IN SCHEMA %I FROM browser_acceptance_probe', schema_name);
    END LOOP;

    EXECUTE format('REVOKE ALL PRIVILEGES ON DATABASE %I FROM browser_acceptance_probe', current_database());
END;
$migration$;

DO $migration$
DECLARE
    default_privilege RECORD;
    object_type TEXT;
BEGIN
    FOR default_privilege IN
        SELECT DISTINCT owner_role.rolname AS owner_name,
                        namespace.nspname AS schema_name,
                        defaults.defaclobjtype AS object_type
        FROM pg_catalog.pg_default_acl AS defaults
        JOIN pg_catalog.pg_roles AS owner_role ON owner_role.oid = defaults.defaclrole
        LEFT JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = defaults.defaclnamespace
        CROSS JOIN LATERAL pg_catalog.aclexplode(defaults.defaclacl) AS privilege
        JOIN pg_catalog.pg_roles AS grantee_role ON grantee_role.oid = privilege.grantee
        WHERE grantee_role.rolname = 'browser_acceptance_probe'
    LOOP
        CASE default_privilege.object_type
            WHEN 'r' THEN object_type := 'TABLES';
            WHEN 'S' THEN object_type := 'SEQUENCES';
            WHEN 'f' THEN object_type := 'FUNCTIONS';
            WHEN 'T' THEN object_type := 'TYPES';
            WHEN 'n' THEN object_type := 'SCHEMAS';
        END CASE;

        EXECUTE format(
            'ALTER DEFAULT PRIVILEGES FOR ROLE %I%s REVOKE ALL ON %s FROM browser_acceptance_probe',
            default_privilege.owner_name,
            CASE WHEN default_privilege.schema_name IS NULL THEN ''
                 ELSE format(' IN SCHEMA %I', default_privilege.schema_name)
            END,
            object_type
        );
    END LOOP;
END;
$migration$;

CREATE FUNCTION public.browser_acceptance_auth_key_bound_count(p_user_id BIGINT)
RETURNS BIGINT
LANGUAGE SQL
STABLE
SECURITY DEFINER
SET search_path = pg_catalog
AS $function$
    SELECT count(*)
    FROM public.auth_keys
    WHERE user_id = p_user_id;
$function$;

CREATE FUNCTION public.browser_acceptance_pending_sign_in_count(p_user_id BIGINT)
RETURNS BIGINT
LANGUAGE SQL
STABLE
SECURITY DEFINER
SET search_path = pg_catalog
AS $function$
    SELECT count(*)
    FROM public.auth_keys
    WHERE pending_user_id = p_user_id;
$function$;

CREATE FUNCTION public.browser_acceptance_bound_auth_key_ids(p_user_id BIGINT)
RETURNS TABLE (id BIGINT, created_at TIMESTAMPTZ)
LANGUAGE SQL
STABLE
SECURITY DEFINER
SET search_path = pg_catalog
AS $function$
    SELECT auth_keys.id, auth_keys.created_at
    FROM public.auth_keys
    WHERE auth_keys.user_id = p_user_id;
$function$;

REVOKE ALL PRIVILEGES ON FUNCTION public.browser_acceptance_auth_key_bound_count(BIGINT) FROM PUBLIC;
REVOKE ALL PRIVILEGES ON FUNCTION public.browser_acceptance_pending_sign_in_count(BIGINT) FROM PUBLIC;
REVOKE ALL PRIVILEGES ON FUNCTION public.browser_acceptance_bound_auth_key_ids(BIGINT) FROM PUBLIC;

GRANT EXECUTE ON FUNCTION public.browser_acceptance_auth_key_bound_count(BIGINT) TO browser_acceptance_probe;
GRANT EXECUTE ON FUNCTION public.browser_acceptance_pending_sign_in_count(BIGINT) TO browser_acceptance_probe;
GRANT EXECUTE ON FUNCTION public.browser_acceptance_bound_auth_key_ids(BIGINT) TO browser_acceptance_probe;

DO $migration$
DECLARE
    owned_object TEXT;
BEGIN
    SELECT pg_catalog.pg_describe_object(dependency.classid, dependency.objid, dependency.objsubid)
    INTO owned_object
    FROM pg_catalog.pg_shdepend AS dependency
    JOIN pg_catalog.pg_roles AS owner_role ON owner_role.oid = dependency.refobjid
    JOIN pg_catalog.pg_database AS database ON database.datname = current_database()
    WHERE dependency.refclassid = 'pg_catalog.pg_authid'::regclass
      AND owner_role.rolname = 'browser_acceptance_probe'
      AND dependency.deptype = 'o'
      AND (
          dependency.dbid = database.oid
          OR (
              dependency.dbid = 0
              AND dependency.classid = 'pg_catalog.pg_database'::regclass
              AND dependency.objid = database.oid
          )
      )
    ORDER BY dependency.classid, dependency.objid, dependency.objsubid
    LIMIT 1;

    IF FOUND THEN
        RAISE EXCEPTION 'browser_acceptance_probe owns %; refusing to provision the probe role', owned_object
            USING ERRCODE = '55000',
                  HINT = 'Transfer this object''s ownership to the existing application owner before retrying.';
    END IF;

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
    membership_grant RECORD;
BEGIN
    FOR membership_grant IN
        SELECT membership.oid AS membership_id,
               member_role.rolname AS member_name,
               grantor_role.rolname AS grantor_name
        FROM pg_catalog.pg_auth_members AS membership
        JOIN pg_catalog.pg_roles AS granted_role ON granted_role.oid = membership.roleid
        JOIN pg_catalog.pg_roles AS member_role ON member_role.oid = membership.member
        JOIN pg_catalog.pg_roles AS grantor_role ON grantor_role.oid = membership.grantor
        WHERE granted_role.rolname = 'browser_acceptance_probe'
    LOOP
        IF EXISTS (
            SELECT 1 FROM pg_catalog.pg_auth_members
            WHERE oid = membership_grant.membership_id
        ) THEN
            -- Dependent grants are also memberships in the probe and must be removed.
            EXECUTE format(
                'REVOKE browser_acceptance_probe FROM %I GRANTED BY %I CASCADE',
                membership_grant.member_name, membership_grant.grantor_name
            );
        END IF;
    END LOOP;

    FOR membership_grant IN
        SELECT granted_role.rolname AS granted_name,
               grantor_role.rolname AS grantor_name
        FROM pg_catalog.pg_auth_members AS membership
        JOIN pg_catalog.pg_roles AS granted_role ON granted_role.oid = membership.roleid
        JOIN pg_catalog.pg_roles AS member_role ON member_role.oid = membership.member
        JOIN pg_catalog.pg_roles AS grantor_role ON grantor_role.oid = membership.grantor
        WHERE member_role.rolname = 'browser_acceptance_probe'
    LOOP
        EXECUTE format(
            'REVOKE %I FROM browser_acceptance_probe GRANTED BY %I',
            membership_grant.granted_name, membership_grant.grantor_name
        );
    END LOOP;

    IF EXISTS (
        SELECT 1
        FROM pg_catalog.pg_auth_members AS membership
        JOIN pg_catalog.pg_roles AS probe_role
          ON probe_role.oid = membership.roleid OR probe_role.oid = membership.member
        WHERE probe_role.rolname = 'browser_acceptance_probe'
    ) THEN
        RAISE EXCEPTION 'browser_acceptance_probe retains role memberships'
            USING ERRCODE = '55000';
    END IF;
END;
$migration$;

DO $migration$
DECLARE
    object_grant RECORD;
    executor_role_name TEXT := current_user;
BEGIN
    FOR object_grant IN
        WITH namespaces AS (
            SELECT oid, nspname, nspacl
            FROM pg_catalog.pg_namespace
            WHERE nspname <> 'information_schema'
              AND pg_catalog.left(nspname, 3) <> 'pg_'
        ), object_acls AS (
            SELECT CASE WHEN relation.relkind = 'S' THEN 'SEQUENCE' ELSE 'TABLE' END AS object_type,
                   format('%I.%I', namespace.nspname, relation.relname) AS object_name,
                   NULL::TEXT AS column_name,
                   relation.relacl AS acl
            FROM pg_catalog.pg_class AS relation
            JOIN namespaces AS namespace ON namespace.oid = relation.relnamespace
            WHERE relation.relkind IN ('r', 'p', 'v', 'm', 'f', 'S')

            UNION ALL

            SELECT 'TABLE', format('%I.%I', namespace.nspname, relation.relname),
                   attribute.attname::TEXT, attribute.attacl
            FROM pg_catalog.pg_attribute AS attribute
            JOIN pg_catalog.pg_class AS relation ON relation.oid = attribute.attrelid
            JOIN namespaces AS namespace ON namespace.oid = relation.relnamespace
            WHERE relation.relkind IN ('r', 'p', 'v', 'm', 'f')
              AND attribute.attnum > 0
              AND NOT attribute.attisdropped

            UNION ALL

            SELECT 'ROUTINE', format('%I.%I(%s)', namespace.nspname, routine.proname,
                                    pg_catalog.pg_get_function_identity_arguments(routine.oid)),
                   NULL::TEXT, routine.proacl
            FROM pg_catalog.pg_proc AS routine
            JOIN namespaces AS namespace ON namespace.oid = routine.pronamespace

            UNION ALL

            SELECT 'SCHEMA', format('%I', nspname), NULL::TEXT, nspacl
            FROM namespaces

            UNION ALL

            SELECT 'DATABASE', format('%I', datname), NULL::TEXT, datacl
            FROM pg_catalog.pg_database
            WHERE datname = current_database()
        )
        SELECT DISTINCT grantor_role.rolname AS grantor_name,
                        object_acls.object_type, object_acls.object_name, object_acls.column_name
        FROM object_acls
        CROSS JOIN LATERAL pg_catalog.aclexplode(object_acls.acl) AS privilege
        JOIN pg_catalog.pg_roles AS grantor_role ON grantor_role.oid = privilege.grantor
        JOIN pg_catalog.pg_roles AS grantee_role ON grantee_role.oid = privilege.grantee
        WHERE grantee_role.rolname = 'browser_acceptance_probe'
    LOOP
        EXECUTE format('SET LOCAL ROLE %I', object_grant.grantor_name);
        EXECUTE format(
            'REVOKE ALL PRIVILEGES%s ON %s %s FROM browser_acceptance_probe',
            CASE WHEN object_grant.column_name IS NULL THEN ''
                 ELSE format(' (%I)', object_grant.column_name)
            END,
            object_grant.object_type,
            object_grant.object_name
        );
        EXECUTE format('SET LOCAL ROLE %I', executor_role_name);
    END LOOP;
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

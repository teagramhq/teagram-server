CREATE FUNCTION public.browser_acceptance_account_high_water_v1()
RETURNS BIGINT
LANGUAGE SQL
VOLATILE
SECURITY DEFINER
SET search_path = pg_catalog
AS $function$
    SELECT CASE
        WHEN sequence.is_called THEN sequence.last_value
        ELSE sequence.last_value - 1
    END
    FROM public.users_id_seq AS sequence;
$function$;

-- This function is the probe's only access to the account ID sequence.
DO $migration$
DECLARE
    object_grant RECORD;
BEGIN
    FOR object_grant IN
        SELECT DISTINCT grantee.rolname AS role_name
        FROM pg_catalog.pg_proc AS routine
        CROSS JOIN LATERAL pg_catalog.aclexplode(
            COALESCE(routine.proacl, pg_catalog.acldefault('f', routine.proowner))
        ) AS privilege
        JOIN pg_catalog.pg_roles AS grantee ON grantee.oid = privilege.grantee
        WHERE routine.oid = 'public.browser_acceptance_account_high_water_v1()'::pg_catalog.regprocedure
          AND privilege.grantee <> routine.proowner
          AND grantee.rolname <> 'browser_acceptance_probe'
    LOOP
        EXECUTE pg_catalog.format(
            'REVOKE ALL PRIVILEGES ON FUNCTION public.browser_acceptance_account_high_water_v1() FROM %I',
            object_grant.role_name
        );
    END LOOP;
END;
$migration$;

REVOKE ALL PRIVILEGES ON FUNCTION public.browser_acceptance_account_high_water_v1()
    FROM PUBLIC, browser_acceptance_probe;
GRANT EXECUTE ON FUNCTION public.browser_acceptance_account_high_water_v1()
    TO browser_acceptance_probe;

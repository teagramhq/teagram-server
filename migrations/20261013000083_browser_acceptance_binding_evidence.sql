-- No account is enrolled by this expansion. Existing bindings are baseline.
CREATE SEQUENCE public.browser_acceptance_binding_epoch_seq AS BIGINT NO CYCLE;

CREATE TABLE public.browser_acceptance_binding_evidence (
    user_id               BIGINT PRIMARY KEY REFERENCES public.users (id) ON DELETE CASCADE,
    evidence_version      INTEGER NOT NULL,
    epoch                 BIGINT NOT NULL CHECK (epoch >= 1),
    additions             BIGINT NOT NULL CHECK (additions >= 0),
    removals              BIGINT NOT NULL CHECK (removals >= 0),
    last_added_created_at TIMESTAMPTZ
);

-- A single evidence row prevents cross-owner sweeps taking evidence locks in
-- opposite orders. The unique index also refuses simultaneous enrollments.
CREATE UNIQUE INDEX browser_acceptance_binding_evidence_single_account
    ON public.browser_acceptance_binding_evidence ((true));

ALTER SEQUENCE public.browser_acceptance_binding_epoch_seq
    OWNED BY public.browser_acceptance_binding_evidence.epoch;

-- Owner-only maintenance. The lock must precede DELETE as well as INSERT:
-- deleting evidence first would invert the auth-key/evidence writer order.
CREATE FUNCTION public.browser_acceptance_enroll_binding_evidence(p_user_id BIGINT)
RETURNS VOID
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $function$
BEGIN
    LOCK TABLE public.auth_keys IN SHARE MODE;

    DELETE FROM public.browser_acceptance_binding_evidence
    WHERE user_id = p_user_id;

    INSERT INTO public.browser_acceptance_binding_evidence (
        user_id, evidence_version, epoch, additions, removals, last_added_created_at
    ) VALUES (
        p_user_id, 1, pg_catalog.nextval('public.browser_acceptance_binding_epoch_seq'), 0, 0, NULL
    );
END;
$function$;

CREATE FUNCTION public.browser_acceptance_record_binding_evidence()
RETURNS TRIGGER
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $function$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF (OLD.id, OLD.created_at, OLD.user_id) IS NOT DISTINCT FROM
           (NEW.id, NEW.created_at, NEW.user_id) THEN
            RETURN NULL;
        END IF;
    END IF;

    IF TG_OP IN ('UPDATE', 'DELETE') AND OLD.user_id IS NOT NULL THEN
        UPDATE public.browser_acceptance_binding_evidence
        SET removals = removals + 1
        WHERE user_id = OLD.user_id;
    END IF;

    IF TG_OP IN ('INSERT', 'UPDATE') AND NEW.user_id IS NOT NULL THEN
        UPDATE public.browser_acceptance_binding_evidence
        SET additions = additions + 1,
            last_added_created_at = NEW.created_at
        WHERE user_id = NEW.user_id;
    END IF;

    RETURN NULL;
END;
$function$;

-- AFTER ROW runs after the statement has locked all affected auth-key rows.
-- The quoted name sorts before RI_ConstraintTrigger_* so DELETE records its
-- evidence before locking SRP challenge rows through their FK cascade.
CREATE TRIGGER "AA_browser_acceptance_binding_evidence"
AFTER INSERT OR UPDATE OR DELETE ON public.auth_keys
FOR EACH ROW
EXECUTE FUNCTION public.browser_acceptance_record_binding_evidence();

ALTER TABLE public.auth_keys ENABLE ALWAYS TRIGGER "AA_browser_acceptance_binding_evidence";

CREATE FUNCTION public.browser_acceptance_reject_auth_keys_truncate()
RETURNS TRIGGER
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $function$
BEGIN
    RAISE EXCEPTION 'TRUNCATE auth_keys is refused while binding evidence is installed'
        USING ERRCODE = '55000';
END;
$function$;

CREATE TRIGGER browser_acceptance_auth_keys_reject_truncate
BEFORE TRUNCATE ON public.auth_keys
FOR EACH STATEMENT
EXECUTE FUNCTION public.browser_acceptance_reject_auth_keys_truncate();

ALTER TABLE public.auth_keys ENABLE ALWAYS TRIGGER browser_acceptance_auth_keys_reject_truncate;

CREATE FUNCTION public.browser_acceptance_binding_snapshot_v1(p_user_id BIGINT)
RETURNS TABLE (
    complete BOOLEAN,
    evidence_version INTEGER,
    epoch BIGINT,
    additions BIGINT,
    removals BIGINT,
    last_added_created_at pg_catalog.timestamptz,
    bound_key_ids BIGINT[],
    bound_key_created_ats pg_catalog.timestamptz[],
    pending_count BIGINT,
    db_time pg_catalog.timestamptz
)
LANGUAGE SQL
STABLE
SECURITY DEFINER
SET search_path = pg_catalog
AS $function$
    SELECT coverage.complete,
           CASE WHEN coverage.complete THEN evidence.evidence_version END,
           CASE WHEN coverage.complete THEN evidence.epoch END,
           CASE WHEN coverage.complete THEN evidence.additions END,
           CASE WHEN coverage.complete THEN evidence.removals END,
           CASE WHEN coverage.complete THEN evidence.last_added_created_at END,
           CASE WHEN coverage.complete THEN keys.ids END,
           CASE WHEN coverage.complete THEN keys.created_ats END,
           CASE WHEN coverage.complete THEN pending.total END,
           CASE WHEN coverage.complete THEN pg_catalog.statement_timestamp() END
    FROM (VALUES (p_user_id)) AS requested(user_id)
    LEFT JOIN public.browser_acceptance_binding_evidence AS evidence
      ON evidence.user_id = requested.user_id
    CROSS JOIN LATERAL (
        SELECT evidence.user_id IS NOT NULL
           AND evidence.evidence_version = 1
           AND EXISTS (
               SELECT 1 FROM pg_catalog.pg_trigger AS trigger
               WHERE trigger.tgrelid = 'public.auth_keys'::pg_catalog.regclass
                 AND trigger.tgname = 'AA_browser_acceptance_binding_evidence'
                 AND trigger.tgfoid = 'public.browser_acceptance_record_binding_evidence()'::pg_catalog.regprocedure
                 AND trigger.tgtype = 29
                 AND trigger.tgenabled = 'A'
           )
           AND EXISTS (
               SELECT 1 FROM pg_catalog.pg_trigger AS trigger
               WHERE trigger.tgrelid = 'public.auth_keys'::pg_catalog.regclass
                 AND trigger.tgname = 'browser_acceptance_auth_keys_reject_truncate'
                 AND trigger.tgfoid = 'public.browser_acceptance_reject_auth_keys_truncate()'::pg_catalog.regprocedure
                 AND trigger.tgtype = 34
                 AND trigger.tgenabled = 'A'
           ) AS complete
    ) AS coverage
    CROSS JOIN LATERAL (
        SELECT COALESCE(pg_catalog.array_agg(auth_key.id ORDER BY auth_key.id), ARRAY[]::BIGINT[]) AS ids,
               COALESCE(pg_catalog.array_agg(auth_key.created_at ORDER BY auth_key.id), ARRAY[]::pg_catalog.timestamptz[]) AS created_ats
        FROM public.auth_keys AS auth_key
        WHERE coverage.complete AND auth_key.user_id = requested.user_id
    ) AS keys
    CROSS JOIN LATERAL (
        SELECT pg_catalog.count(*) AS total
        FROM public.auth_keys AS auth_key
        WHERE coverage.complete AND auth_key.pending_user_id = requested.user_id
    ) AS pending;
$function$;

REVOKE ALL PRIVILEGES ON TABLE public.browser_acceptance_binding_evidence FROM PUBLIC, browser_acceptance_probe;
REVOKE ALL PRIVILEGES ON SEQUENCE public.browser_acceptance_binding_epoch_seq FROM PUBLIC, browser_acceptance_probe;
REVOKE ALL PRIVILEGES ON FUNCTION public.browser_acceptance_enroll_binding_evidence(BIGINT)
    FROM PUBLIC, browser_acceptance_probe;
REVOKE ALL PRIVILEGES ON FUNCTION public.browser_acceptance_record_binding_evidence()
    FROM PUBLIC, browser_acceptance_probe;
REVOKE ALL PRIVILEGES ON FUNCTION public.browser_acceptance_reject_auth_keys_truncate()
    FROM PUBLIC, browser_acceptance_probe;
REVOKE ALL PRIVILEGES ON FUNCTION public.browser_acceptance_binding_snapshot_v1(BIGINT)
    FROM PUBLIC, browser_acceptance_probe;

-- Remove any other grants inherited from the migration owner's default ACLs.
-- These objects are owner-only except for the explicit probe read grant below.
DO $migration$
DECLARE
    object_grant RECORD;
BEGIN
    FOR object_grant IN
        WITH object_acls AS (
            SELECT CASE WHEN relation.relkind = 'S' THEN 'SEQUENCE' ELSE 'TABLE' END AS object_type,
                   pg_catalog.format('%I.%I', namespace.nspname, relation.relname) AS object_name,
                   relation.relacl AS acl, relation.relowner AS owner
            FROM pg_catalog.pg_class AS relation
            JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
            WHERE namespace.nspname = 'public'
              AND relation.relname IN ('browser_acceptance_binding_evidence', 'browser_acceptance_binding_epoch_seq')

            UNION ALL

            SELECT 'FUNCTION', pg_catalog.format('%I.%I(%s)', namespace.nspname, routine.proname,
                                                pg_catalog.pg_get_function_identity_arguments(routine.oid)),
                   routine.proacl, routine.proowner
            FROM pg_catalog.pg_proc AS routine
            JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = routine.pronamespace
            WHERE namespace.nspname = 'public'
              AND routine.proname IN (
                  'browser_acceptance_enroll_binding_evidence', 'browser_acceptance_record_binding_evidence',
                  'browser_acceptance_reject_auth_keys_truncate', 'browser_acceptance_binding_snapshot_v1'
              )
        )
        SELECT DISTINCT object_acls.object_type, object_acls.object_name, grantee.rolname
        FROM object_acls
        CROSS JOIN LATERAL pg_catalog.aclexplode(object_acls.acl) AS privilege
        JOIN pg_catalog.pg_roles AS grantee ON grantee.oid = privilege.grantee
        WHERE privilege.grantee <> object_acls.owner
    LOOP
        EXECUTE pg_catalog.format('REVOKE ALL PRIVILEGES ON %s %s FROM %I',
                                  object_grant.object_type, object_grant.object_name, object_grant.rolname);
    END LOOP;
END;
$migration$;

GRANT EXECUTE ON FUNCTION public.browser_acceptance_binding_snapshot_v1(BIGINT) TO browser_acceptance_probe;

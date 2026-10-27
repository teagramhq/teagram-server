-- Permit a resolver and an import probe for the same number at the same time.
-- atlas:txmode none

CREATE UNIQUE INDEX CONCURRENTLY phone_lookups_caller_type_phone_time
    ON phone_lookups (caller_id, lookup_type, phone, looked_up_at);

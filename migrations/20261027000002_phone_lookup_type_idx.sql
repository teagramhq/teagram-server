-- Build the type-aware quota index without blocking active lookups.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY phone_lookups_caller_type_time
    ON phone_lookups (caller_id, lookup_type, looked_up_at);

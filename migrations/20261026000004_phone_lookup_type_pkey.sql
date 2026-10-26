-- Let resolver and contact-import lookups record the same phone and timestamp
-- independently in the shared lookup ledger.
ALTER TABLE phone_lookups DROP CONSTRAINT phone_lookups_pkey;
ALTER TABLE phone_lookups
    ADD CONSTRAINT phone_lookups_pkey PRIMARY KEY USING INDEX phone_lookups_caller_type_phone_time;

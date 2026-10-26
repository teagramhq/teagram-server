-- Keep contact-import probes on the existing rolling lookup ledger without
-- changing the independent contacts.resolvePhone quota.
ALTER TABLE phone_lookups
    ADD COLUMN lookup_type TEXT NOT NULL DEFAULT 'resolve_phone';

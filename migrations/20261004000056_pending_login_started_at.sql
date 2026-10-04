-- A pending 2FA login's lease is shared by every replica and starts when
-- auth.signIn stages the pending user. NULL on legacy rows means fail closed.
ALTER TABLE auth_keys ADD COLUMN pending_started_at TIMESTAMPTZ NULL;

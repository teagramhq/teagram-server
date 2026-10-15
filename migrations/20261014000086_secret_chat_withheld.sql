-- A secret-chat request from a user the participant has blocked is stored for
-- the initiator only. The flag is decided at insert and never recomputed from
-- blocked_users, so a later unblock does not reveal the request.

ALTER TABLE secret_chats ADD COLUMN withheld BOOLEAN NOT NULL DEFAULT false;

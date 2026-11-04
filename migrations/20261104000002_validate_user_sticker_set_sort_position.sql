-- Validates the sort_position check added by migration 20261104000001. The
-- table scan runs after that migration commits and takes only a lock that is
-- compatible with ordinary reads and writes.
ALTER TABLE user_sticker_sets VALIDATE CONSTRAINT user_sticker_sets_sort_position_positive;

-- Saved order of an account's installed sticker sets. NULL means the set has
-- never been placed by a reorder and keeps set_id order after the placed sets.
-- Expand-only: a nullable column without a default is metadata-only, and
-- dropping the column is the rollback. The check is added NOT VALID so the
-- table is not scanned while locked; migration 20261104000002 validates it.
ALTER TABLE user_sticker_sets ADD COLUMN sort_position INT;
ALTER TABLE user_sticker_sets
    ADD CONSTRAINT user_sticker_sets_sort_position_positive
    CHECK (sort_position > 0) NOT VALID;

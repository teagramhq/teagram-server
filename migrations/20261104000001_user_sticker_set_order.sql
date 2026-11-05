-- Account-chosen order of installed sticker sets. NULL keeps the set_id order
-- every account sees today, and a reorder assigns positions to all installed
-- sets. A nullable column with no default rewrites no rows and needs no
-- backfill. Rollback is dropping the column, which only loses the chosen
-- order and restores set_id order.

ALTER TABLE user_sticker_sets
    ADD COLUMN sort_position INT NULL CHECK (sort_position > 0);

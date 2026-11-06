-- The open-report identity index now permits distinct and renewed reports.
ALTER TABLE user_sticker_set_reports
    DROP CONSTRAINT user_sticker_set_reports_reporter_set_key;

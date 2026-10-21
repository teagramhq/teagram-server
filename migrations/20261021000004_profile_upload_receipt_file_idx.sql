-- Chat-photo eligibility rejects files still named by a profile-upload receipt,
-- including gallery uploads whose entry was later removed. This reverse index
-- keeps that per-file check bounded as receipt history grows.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY profile_upload_receipt_file_id_idx
    ON profile_upload_receipt (file_id)
    WHERE file_id IS NOT NULL;

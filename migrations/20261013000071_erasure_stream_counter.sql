-- Transactional positions for the inert erasure outbox verification seam.
-- Each row is the last committed position in one (epoch, stream). A transaction
-- updates it together with its outbox record, so rollback cannot create a gap.
-- There is intentionally no database sequence or default-generated value.
-- Nothing in a live path allocates from this table.
CREATE TABLE erasure_stream_counter (
    epoch         BIGINT NOT NULL CHECK (epoch >= 1),
    stream_id     BYTEA  NOT NULL CHECK (octet_length(stream_id) = 16),
    last_position BIGINT NOT NULL CHECK (last_position >= 0),
    PRIMARY KEY (epoch, stream_id)
);

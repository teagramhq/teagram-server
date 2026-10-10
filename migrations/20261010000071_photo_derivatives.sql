-- Inert storage for bounded photo derivatives. Existing uploads and readers do
-- not write or consult this table; a later writer can publish a complete row
-- before setting the parent file's stored flag.
CREATE TABLE photo_derivatives (
    file_id   BIGINT  NOT NULL PRIMARY KEY REFERENCES files (id) ON DELETE CASCADE,
    m_width   INTEGER NULL,
    m_height  INTEGER NULL,
    m_size    INTEGER NULL,
    m_bytes   BYTEA   NULL,
    stripped  BYTEA   NOT NULL,
    CONSTRAINT photo_derivatives_m_fields_present CHECK (
        (m_width IS NULL AND m_height IS NULL AND m_size IS NULL AND m_bytes IS NULL)
        OR
        (m_width IS NOT NULL AND m_height IS NOT NULL AND m_size IS NOT NULL AND m_bytes IS NOT NULL)
    ),
    CONSTRAINT photo_derivatives_m_size_valid CHECK (
        m_size IS NULL OR (
            m_size BETWEEN 1 AND 65536
            AND m_size = octet_length(m_bytes)
        )
    ),
    CONSTRAINT photo_derivatives_m_dimensions_valid CHECK (
        m_width IS NULL OR (
            m_width BETWEEN 1 AND 320
            AND m_height BETWEEN 1 AND 320
            AND GREATEST(m_width, m_height) = 320
        )
    ),
    CONSTRAINT photo_derivatives_stripped_valid CHECK (
        CASE
            WHEN octet_length(stripped) BETWEEN 3 AND 2048 THEN
                get_byte(stripped, 0) = 1
                AND get_byte(stripped, 1) BETWEEN 1 AND 40
                AND get_byte(stripped, 2) BETWEEN 1 AND 40
            ELSE false
        END
    )
);

-- The file row is the publication interlock. Taking its NO KEY UPDATE lock
-- makes this check serialize with stored=true publication while preserving the
-- established file-reference-before-file-row order. A child delete is allowed
-- only before publication or when it is being invoked by the parent's cascade.
CREATE FUNCTION photo_derivatives_guard_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    parent_stored BOOLEAN;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        RAISE EXCEPTION USING
            ERRCODE = '23514',
            CONSTRAINT = 'photo_derivatives_immutable',
            MESSAGE = 'photo derivatives cannot be updated';
    END IF;

    IF TG_OP = 'DELETE' THEN
        IF pg_trigger_depth() > 1 THEN
            RETURN OLD;
        END IF;

        SELECT f.stored INTO parent_stored
        FROM files f
        WHERE f.id = OLD.file_id
        FOR NO KEY UPDATE;
        IF FOUND AND parent_stored THEN
            RAISE EXCEPTION USING
                ERRCODE = '23514',
                CONSTRAINT = 'photo_derivatives_immutable',
                MESSAGE = 'published photo derivatives cannot be deleted';
        END IF;
        RETURN OLD;
    END IF;

    SELECT f.stored INTO parent_stored
    FROM files f
    WHERE f.id = NEW.file_id
    FOR NO KEY UPDATE;
    IF NOT FOUND OR parent_stored THEN
        RAISE EXCEPTION USING
            ERRCODE = '23514',
            CONSTRAINT = 'photo_derivatives_parent_unstored',
            MESSAGE = 'photo derivatives require an unstored parent file';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER photo_derivatives_guard_mutation
BEFORE INSERT OR UPDATE OR DELETE ON photo_derivatives
FOR EACH ROW EXECUTE FUNCTION photo_derivatives_guard_mutation();

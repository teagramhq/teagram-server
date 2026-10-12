ALTER TABLE users
    ADD COLUMN authorization_ttl_days INT NOT NULL DEFAULT 365
    CHECK (authorization_ttl_days IN (7, 30, 90, 180, 365));

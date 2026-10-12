ALTER TABLE auth_keys
    ADD COLUMN device_model TEXT NULL CHECK (octet_length(device_model) <= 128),
    ADD COLUMN platform TEXT NULL CHECK (octet_length(platform) <= 128),
    ADD COLUMN system_version TEXT NULL CHECK (octet_length(system_version) <= 128),
    ADD COLUMN api_id INTEGER NULL,
    ADD COLUMN app_version TEXT NULL CHECK (octet_length(app_version) <= 128),
    ADD COLUMN ip TEXT NULL CHECK (octet_length(ip) <= 128);

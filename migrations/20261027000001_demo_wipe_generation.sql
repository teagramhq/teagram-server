CREATE TABLE demo_wipe_generation (
  singleton SMALLINT PRIMARY KEY CHECK (singleton = 1),
  generation TEXT NOT NULL CHECK (generation ~ '^[0-9a-f]{64}$' AND length(generation) = 64)
);

INSERT INTO demo_wipe_generation (singleton, generation)
VALUES (
  1,
  replace(gen_random_uuid()::text, '-', '') || replace(gen_random_uuid()::text, '-', '')
);

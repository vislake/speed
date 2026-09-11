-- The probe table the engine's migration-stage test asserts on: it exists
-- only if a fake module's migration file was registered and applied.
CREATE TABLE IF NOT EXISTS engine_probe (
    id TEXT NOT NULL PRIMARY KEY
);

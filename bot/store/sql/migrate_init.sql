CREATE TABLE IF NOT EXISTS bot_schema_migrations(version TEXT PRIMARY KEY,applied_at TIMESTAMPTZ NOT NULL DEFAULT now());

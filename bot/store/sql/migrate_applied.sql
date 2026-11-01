SELECT EXISTS(SELECT 1 FROM bot_schema_migrations WHERE version=$1);

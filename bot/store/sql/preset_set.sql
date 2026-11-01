INSERT INTO bot_presets(bot_id,user_id,preset) VALUES($1,$2,$3) ON CONFLICT(bot_id,user_id) DO UPDATE SET preset=EXCLUDED.preset;

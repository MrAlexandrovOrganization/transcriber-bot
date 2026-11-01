INSERT INTO transcription_deliveries(bot_id,update_id,part,kind,text,quoted) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING;

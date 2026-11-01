INSERT INTO transcription_jobs(bot_id,update_id,chat_id,user_id,message_id,file_id,format,preset,noun,request_id)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT DO NOTHING;

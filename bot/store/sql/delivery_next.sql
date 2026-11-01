SELECT part,kind,text,message_id,quoted FROM transcription_deliveries WHERE bot_id=$1 AND update_id=$2 AND NOT delivered ORDER BY part LIMIT 1;

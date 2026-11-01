UPDATE transcription_jobs SET next_attempt_at=now()+($3 * INTERVAL '1 second'),attempts=CASE WHEN $4='' THEN 0 ELSE attempts+1 END,last_error=$4,updated_at=now() WHERE bot_id=$1 AND update_id=$2;

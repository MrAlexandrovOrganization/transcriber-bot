UPDATE transcription_jobs SET cancel_requested=true,updated_at=now()
WHERE bot_id=$1 AND update_id=$2 AND state='poll';

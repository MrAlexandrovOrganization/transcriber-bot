UPDATE transcription_jobs SET acknowledged=true,updated_at=now() WHERE bot_id=$1 AND update_id=$2;

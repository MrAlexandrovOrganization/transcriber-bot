UPDATE transcription_jobs SET status_message_id=$3,state='upload',attempts=0,last_error='',next_attempt_at=now(),updated_at=now() WHERE bot_id=$1 AND update_id=$2;

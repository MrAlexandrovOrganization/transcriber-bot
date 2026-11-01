UPDATE transcription_jobs SET cancel_requested=true,next_attempt_at=now() WHERE bot_id=$1 AND job_id=$2 AND chat_id=$3 AND state='poll';

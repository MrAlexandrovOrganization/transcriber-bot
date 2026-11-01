UPDATE transcription_jobs SET result=$3,state='deliver',acknowledged=(job_id=''),attempts=0,last_error='',next_attempt_at=now(),updated_at=now() WHERE bot_id=$1 AND update_id=$2;

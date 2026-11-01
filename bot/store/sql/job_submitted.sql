UPDATE transcription_jobs SET job_id=$3,state='poll',attempts=0,last_error='',next_attempt_at=now(),updated_at=now() WHERE bot_id=$1 AND update_id=$2;

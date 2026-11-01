SELECT j.update_id FROM transcription_jobs j
WHERE j.bot_id=$1 AND j.state=$2 AND j.state<>'done' AND j.next_attempt_at<=now()
AND ($2 NOT IN ('status','upload') OR NOT EXISTS (
 SELECT 1 FROM transcription_jobs earlier WHERE earlier.bot_id=j.bot_id AND earlier.chat_id=j.chat_id
 AND earlier.state IN ('status','upload') AND earlier.update_id<j.update_id AND (earlier.state=$2 OR ($2='upload' AND earlier.state='status'))))
ORDER BY j.next_attempt_at,j.update_id LIMIT 32;

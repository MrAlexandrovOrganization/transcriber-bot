SELECT bot_id,update_id,chat_id,user_id,message_id,file_id,format,preset,noun,request_id,job_id,
status_message_id,state,result,acknowledged,cancel_requested,progress_text,attempts,next_attempt_at,next_attempt_at<=now() AS ready
FROM transcription_jobs WHERE bot_id=$1 AND update_id=$2;

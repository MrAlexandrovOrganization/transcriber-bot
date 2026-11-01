UPDATE transcription_deliveries SET delivered=true,message_id=$4 WHERE bot_id=$1 AND update_id=$2 AND part=$3;

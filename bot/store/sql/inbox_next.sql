SELECT update_id,payload FROM bot_inbox WHERE bot_id=$1 AND NOT handled ORDER BY update_id LIMIT 1;

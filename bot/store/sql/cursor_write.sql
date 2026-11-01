UPDATE bot_cursors SET next_update_id=GREATEST(next_update_id,$2),last_poll_at=now() WHERE bot_id=$1;

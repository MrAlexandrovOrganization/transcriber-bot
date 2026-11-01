UPDATE bot_inbox SET handled=true,payload='{}' WHERE bot_id=$1 AND update_id=$2;

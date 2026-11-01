SELECT COALESCE((SELECT preset FROM bot_presets WHERE bot_id=$1 AND user_id=$2),'auto');

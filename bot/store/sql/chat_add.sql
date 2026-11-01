INSERT INTO chats(chat_id,title,added_by) VALUES($1,$2,$3) ON CONFLICT DO NOTHING;

ALTER TABLE transcription_jobs ADD COLUMN group_chat BOOLEAN NOT NULL DEFAULT false;
-- Existing jobs only come from private chats and groups/supergroups.
UPDATE transcription_jobs SET group_chat=true WHERE chat_id < 0;

ALTER TABLE transcription_deliveries DROP CONSTRAINT transcription_deliveries_kind_check;
ALTER TABLE transcription_deliveries ADD CONSTRAINT transcription_deliveries_kind_check
    CHECK (kind IN ('edit','send','document','delete'));

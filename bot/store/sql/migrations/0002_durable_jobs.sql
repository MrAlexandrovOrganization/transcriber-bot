CREATE TABLE bot_cursors (bot_id BIGINT PRIMARY KEY, next_update_id BIGINT NOT NULL DEFAULT 0, last_poll_at TIMESTAMPTZ);
CREATE TABLE bot_inbox (
 bot_id BIGINT NOT NULL, update_id BIGINT NOT NULL, payload JSONB NOT NULL,
 handled BOOLEAN NOT NULL DEFAULT false, received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 PRIMARY KEY (bot_id, update_id)
);
CREATE INDEX bot_inbox_pending ON bot_inbox (bot_id, update_id) WHERE NOT handled;
CREATE TABLE bot_presets (bot_id BIGINT NOT NULL, user_id BIGINT NOT NULL, preset TEXT NOT NULL, PRIMARY KEY(bot_id,user_id));
CREATE TABLE transcription_jobs (
 bot_id BIGINT NOT NULL, update_id BIGINT NOT NULL, chat_id BIGINT NOT NULL,
 user_id BIGINT NOT NULL, message_id INTEGER NOT NULL, file_id TEXT NOT NULL,
 format TEXT NOT NULL, preset TEXT NOT NULL, noun TEXT NOT NULL,
 request_id TEXT NOT NULL UNIQUE, job_id TEXT NOT NULL DEFAULT '',
 status_message_id INTEGER NOT NULL DEFAULT 0,
 state TEXT NOT NULL DEFAULT 'status' CHECK (state IN ('status','upload','poll','deliver','done')),
 result JSONB, acknowledged BOOLEAN NOT NULL DEFAULT false,
 cancel_requested BOOLEAN NOT NULL DEFAULT false,
 progress_text TEXT NOT NULL DEFAULT '', attempts INTEGER NOT NULL DEFAULT 0,
 next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(), last_error TEXT NOT NULL DEFAULT '',
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 PRIMARY KEY(bot_id,update_id), UNIQUE(bot_id,chat_id,message_id)
);
CREATE INDEX transcription_jobs_pending ON transcription_jobs (bot_id,state,next_attempt_at,update_id) WHERE state <> 'done';
CREATE INDEX transcription_jobs_admission ON transcription_jobs(bot_id,chat_id,update_id) WHERE state IN ('status','upload');
CREATE TABLE transcription_deliveries (
 bot_id BIGINT NOT NULL, update_id BIGINT NOT NULL, part INTEGER NOT NULL,
 quoted BOOLEAN NOT NULL DEFAULT false,
 kind TEXT NOT NULL CHECK(kind IN ('edit','send','document')), text TEXT NOT NULL,
 message_id INTEGER NOT NULL DEFAULT 0, delivered BOOLEAN NOT NULL DEFAULT false,
 PRIMARY KEY(bot_id,update_id,part),
 FOREIGN KEY(bot_id,update_id) REFERENCES transcription_jobs(bot_id,update_id)
);

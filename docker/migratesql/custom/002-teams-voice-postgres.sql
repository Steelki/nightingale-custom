-- 002: Teams voice call records; apply once after 001, or use runtime AutoMigrate.
CREATE TABLE "response_teams_voice_call" ("id" varchar(36),"dedup" varchar(64),"episode" varchar(160),"channel_id" bigint,"rule_id" bigint,"group_id" bigint,"recipient" varchar(320),"title" text,"state" varchar(24),"call_id" varchar(128),"answered_at" bigint,"play_state" varchar(24),"ended_at" bigint,"reason" text,"created_at" bigint,"expires_at" bigint,"fallback_queued" boolean,"close_requested" boolean,"audio_token" varchar(64),"payload" text,PRIMARY KEY ("id"));
CREATE INDEX IF NOT EXISTS "idx_response_teams_voice_call_episode" ON "response_teams_voice_call" ("episode");
CREATE INDEX IF NOT EXISTS "idx_response_teams_voice_call_expires_at" ON "response_teams_voice_call" ("expires_at");
CREATE INDEX IF NOT EXISTS "idx_response_teams_voice_call_state" ON "response_teams_voice_call" ("state");
CREATE UNIQUE INDEX IF NOT EXISTS "idx_response_teams_voice_call_dedup" ON "response_teams_voice_call" ("dedup");

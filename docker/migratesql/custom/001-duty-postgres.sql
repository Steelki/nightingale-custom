-- 001: upstream v9.3.0 -> custom duty schema. Back up first.
-- Run once only; existing custom installations use runtime AutoMigrate.
-- Requires a fully initialized upstream database with notify_rule present.
ALTER TABLE notify_rule ADD COLUMN receiver_mode varchar(20) DEFAULT 'original';
ALTER TABLE notify_rule ADD COLUMN duty_group_id bigint DEFAULT 0;
CREATE TABLE "response_claim_notice" ("group_id" bigint,"id" bigserial,"dedup_key" varchar(64),"response_id" bigint,"event_id" bigint,"group_name" text,"title" text,"claimant" text,"claimed_at" bigint,"timezone" text,"destination" text,"status" varchar(20),"started_at" bigint,"finished_at" bigint,"reason" text,PRIMARY KEY ("id"));
CREATE TABLE "response_duty_audit" ("id" bigserial,"group_id" bigint,"actor" text,"action" text,"version" bigint,"created_at" bigint,PRIMARY KEY ("id"));
CREATE TABLE "response_duty_group" ("id" bigserial,"name" varchar(80),"config" text,"secrets" text,"version" bigint,"updated_at" bigint,"updated_by" text,PRIMARY KEY ("id"));
CREATE TABLE "response_duty_notice" ("id" bigserial,"dedup_key" varchar(64),"kind" text,"group_id" bigint,"response_id" bigint,"assignment_version" bigint,"rule_id" bigint,"channel_id" bigint,"config_index" bigint,"recipient" text,"status" varchar(20),"reason" text,"payload" text,"created_at" bigint,"started_at" bigint,"finished_at" bigint,"expires_at" bigint,PRIMARY KEY ("id"));
CREATE TABLE "response_duty_reminder_clock" ("group_id" bigserial,"next_at" bigint,PRIMARY KEY ("group_id"));
CREATE TABLE "response_duty_settings" ("id" bigserial,"fallback" text,"version" bigint,"secrets" text,PRIMARY KEY ("id"));
CREATE TABLE "response_event" ("assignment_version" bigint,"id" bigserial,"episode" varchar(160),"hash" varchar(128),"first_trigger_time" bigint,"owner_id" bigint,"owner_name" text,"recovered" boolean,"unclaimed_since" bigint,"version" bigint,"updated_at" bigint,PRIMARY KEY ("id"));
CREATE TABLE "response_event_log" ("id" bigserial,"response_id" bigint,"actor" text,"action" text,"content" text,"created_at" bigint,PRIMARY KEY ("id"));
CREATE INDEX IF NOT EXISTS "idx_response_claim_notice_response_id" ON "response_claim_notice" ("response_id");
CREATE INDEX IF NOT EXISTS "idx_response_claim_notice_status" ON "response_claim_notice" ("status");
CREATE INDEX IF NOT EXISTS "idx_response_duty_audit_group_id" ON "response_duty_audit" ("group_id");
CREATE INDEX IF NOT EXISTS "idx_response_duty_notice_group_id" ON "response_duty_notice" ("group_id");
CREATE INDEX IF NOT EXISTS "idx_response_duty_notice_response_id" ON "response_duty_notice" ("response_id");
CREATE INDEX IF NOT EXISTS "idx_response_duty_notice_status" ON "response_duty_notice" ("status");
CREATE INDEX IF NOT EXISTS "idx_response_event_hash" ON "response_event" ("hash");
CREATE INDEX IF NOT EXISTS "idx_response_event_log_response_id" ON "response_event_log" ("response_id");
CREATE UNIQUE INDEX IF NOT EXISTS "idx_response_claim_notice_dedup_key" ON "response_claim_notice" ("dedup_key");
CREATE UNIQUE INDEX IF NOT EXISTS "idx_response_duty_group_name" ON "response_duty_group" ("name");
CREATE UNIQUE INDEX IF NOT EXISTS "idx_response_duty_notice_dedup_key" ON "response_duty_notice" ("dedup_key");
CREATE UNIQUE INDEX IF NOT EXISTS "idx_response_event_episode" ON "response_event" ("episode");

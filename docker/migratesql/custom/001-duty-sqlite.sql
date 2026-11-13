-- 001: upstream v9.3.0 -> custom duty schema. Back up first.
-- Run once only; existing custom installations use runtime AutoMigrate.
-- Requires a fully initialized upstream database with notify_rule present.
ALTER TABLE notify_rule ADD COLUMN receiver_mode varchar(20) DEFAULT 'original';
ALTER TABLE notify_rule ADD COLUMN duty_group_id bigint DEFAULT 0;
CREATE TABLE `response_claim_notice` (`group_id` integer,`id` integer PRIMARY KEY AUTOINCREMENT,`dedup_key` text,`response_id` integer,`event_id` integer,`group_name` text,`title` text,`claimant` text,`claimed_at` integer,`timezone` text,`destination` text,`status` text,`started_at` integer,`finished_at` integer,`reason` text);
CREATE TABLE `response_duty_audit` (`id` integer PRIMARY KEY AUTOINCREMENT,`group_id` integer,`actor` text,`action` text,`version` integer,`created_at` integer);
CREATE TABLE `response_duty_group` (`id` integer PRIMARY KEY AUTOINCREMENT,`name` text,`config` text,`secrets` text,`version` integer,`updated_at` integer,`updated_by` text);
CREATE TABLE `response_duty_notice` (`id` integer PRIMARY KEY AUTOINCREMENT,`dedup_key` text,`kind` text,`group_id` integer,`response_id` integer,`assignment_version` integer,`rule_id` integer,`channel_id` integer,`config_index` integer,`recipient` text,`status` text,`reason` text,`payload` text,`created_at` integer,`started_at` integer,`finished_at` integer,`expires_at` integer);
CREATE TABLE `response_duty_reminder_clock` (`group_id` integer PRIMARY KEY AUTOINCREMENT,`next_at` integer);
CREATE TABLE `response_duty_settings` (`id` integer PRIMARY KEY AUTOINCREMENT,`fallback` text,`version` integer,`secrets` text);
CREATE TABLE `response_event_log` (`id` integer PRIMARY KEY AUTOINCREMENT,`response_id` integer,`actor` text,`action` text,`content` text,`created_at` integer);
CREATE TABLE `response_event` (`assignment_version` integer,`id` integer PRIMARY KEY AUTOINCREMENT,`episode` text,`hash` text,`first_trigger_time` integer,`owner_id` integer,`owner_name` text,`recovered` numeric,`unclaimed_since` integer,`version` integer,`updated_at` integer);
CREATE INDEX `idx_response_claim_notice_response_id` ON `response_claim_notice`(`response_id`);
CREATE INDEX `idx_response_claim_notice_status` ON `response_claim_notice`(`status`);
CREATE INDEX `idx_response_duty_audit_group_id` ON `response_duty_audit`(`group_id`);
CREATE INDEX `idx_response_duty_notice_group_id` ON `response_duty_notice`(`group_id`);
CREATE INDEX `idx_response_duty_notice_response_id` ON `response_duty_notice`(`response_id`);
CREATE INDEX `idx_response_duty_notice_status` ON `response_duty_notice`(`status`);
CREATE INDEX `idx_response_event_hash` ON `response_event`(`hash`);
CREATE INDEX `idx_response_event_log_response_id` ON `response_event_log`(`response_id`);
CREATE UNIQUE INDEX `idx_response_claim_notice_dedup_key` ON `response_claim_notice`(`dedup_key`);
CREATE UNIQUE INDEX `idx_response_duty_group_name` ON `response_duty_group`(`name`);
CREATE UNIQUE INDEX `idx_response_duty_notice_dedup_key` ON `response_duty_notice`(`dedup_key`);
CREATE UNIQUE INDEX `idx_response_event_episode` ON `response_event`(`episode`);

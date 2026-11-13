-- 002: Teams voice call records; apply once after 001, or use runtime AutoMigrate.
CREATE TABLE `response_teams_voice_call` (`id` text,`dedup` text,`episode` text,`channel_id` integer,`rule_id` integer,`group_id` integer,`recipient` text,`title` text,`state` text,`call_id` text,`answered_at` integer,`play_state` text,`ended_at` integer,`reason` text,`created_at` integer,`expires_at` integer,`fallback_queued` numeric,`close_requested` numeric,`audio_token` text,`payload` text,PRIMARY KEY (`id`));
CREATE INDEX `idx_response_teams_voice_call_episode` ON `response_teams_voice_call`(`episode`);
CREATE INDEX `idx_response_teams_voice_call_expires_at` ON `response_teams_voice_call`(`expires_at`);
CREATE INDEX `idx_response_teams_voice_call_state` ON `response_teams_voice_call`(`state`);
CREATE UNIQUE INDEX `idx_response_teams_voice_call_dedup` ON `response_teams_voice_call`(`dedup`);

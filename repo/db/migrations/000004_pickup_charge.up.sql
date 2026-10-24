CREATE TABLE IF NOT EXISTS `pickup_charges` (`id` integer PRIMARY KEY AUTOINCREMENT,`created_at` datetime,`updated_at` datetime,`deleted_at` datetime,`code` text NOT NULL,`token_hash` text NOT NULL,`channel` text,`charged_at` datetime);
CREATE UNIQUE INDEX IF NOT EXISTS `idx_pickup_charge_code_token` ON `pickup_charges`(`code`,`token_hash`);
CREATE INDEX IF NOT EXISTS `idx_pickup_charges_charged_at` ON `pickup_charges`(`charged_at`);

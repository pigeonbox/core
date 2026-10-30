-- API Key 权限范围（2026-10-30）：JSON 数组文本，空串 = 全量（与引入前行为一致）。
ALTER TABLE `user_api_keys` ADD COLUMN `scopes` text;

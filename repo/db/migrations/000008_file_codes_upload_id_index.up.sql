-- 2026-11-15 issue #184：file_codes.upload_id 补非唯一索引。
--
-- 缺陷：该列无任何索引（000001 只建 user_id / code / deleted_at 三索引）
--   ⇒ 上传完成后的读回按 upload_id 查询走全表扫（行数随分享量线性涨）。
--
-- ⇒ 加非唯一索引。**刻意不建唯一**：upload_id 可为空、复用语义未定，
--   「upload_id 唯一索引」是 wave1 计划明令禁止项。
CREATE INDEX IF NOT EXISTS `idx_file_codes_upload_id`
    ON `file_codes` (`upload_id`);

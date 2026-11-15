-- 回滚：去掉 file_codes.upload_id 索引（回到全表扫，仅排障用）。
DROP INDEX IF EXISTS `idx_file_codes_upload_id`;

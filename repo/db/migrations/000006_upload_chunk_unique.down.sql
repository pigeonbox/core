-- 回滚：去掉 (upload_id, chunk_index) 唯一索引。
--
-- ⚠️ 回滚后重复行可能再次出现（分片重传会建新行）⇒ 回滚仅用于排障，
-- 生产不建议回滚。
DROP INDEX IF EXISTS `idx_upload_chunks_upload_id_chunk_index`;

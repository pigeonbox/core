-- 2026-10-25 审计 A3：分片行补 (upload_id, chunk_index) 唯一索引。
--
-- 缺陷：`UploadChunk` 无该唯一索引 ⇒ 同一分片索引可有多行：
--   · dao.GetChunkByIndex 的 WHERE 含 `completed = true` ⇒ **未完成的行查不到**
--     ⇒ 分片失败后重传同索引会**再建一行**（service 的 Update 分支实为死代码）；
--   · dao.CountCompletedChunks 按**行数**计 ⇒ 「分片齐全」判定可虚高
--     （声明 5 片只传 4 片也可能计成 5）⇒ 合并时才发现缺片。
--
-- ⇒ 同一 (upload_id, chunk_index) 只允许一行 ⇒ 从根上消除重复行。
-- （上线前已核对生产数据：0 组重复 ⇒ 本迁移不需要去重。）
--
-- 说明：chunk_index = -1 是**控制行**（每个 upload 一条）⇒ 同一约束也覆盖它。
CREATE UNIQUE INDEX IF NOT EXISTS `idx_upload_chunks_upload_id_chunk_index`
    ON `upload_chunks` (`upload_id`, `chunk_index`);

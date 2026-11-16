-- 2026-11-16 issue #192：对象 GC 持久化任务表（gc_jobs）。
--
-- 缺陷：删除链路此前是「删库 → 尝试同步删对象 → 失败就等下一次全量对账」，
-- 无持久化任务面：进程在「删库成功、删对象前」崩溃 ⇒ 远端对象永久泄漏；
-- 远端 5xx/限流无重试租约；积压不可观测。
--
-- ⇒ gc_jobs（Transactional Outbox）：与业务删除同事务写任务，worker 按租约
-- 消费（state 机 + attempts + next_attempt_at 退避 + lease_until 租约 +
-- dead 死信）。多副本只有一个 worker 跑（bootstrap 仅 admin 面装配）+
-- 行级 CAS 抢占。
--
-- 刻意不建 deleted_at 列（任务行无软删语义，状态机即生命周期）；
-- AutoMigrate 按 model.GCJob 为真相源，本文件是升级路径的显式留痕
-- （同 000004 先例：SQLite 方言，两套机制并存）。
CREATE TABLE IF NOT EXISTS `gc_jobs` (`id` integer PRIMARY KEY AUTOINCREMENT,`created_at` datetime,`updated_at` datetime,`object_key` text,`backend` text,`reason` text,`state` text DEFAULT 'pending',`attempts` integer DEFAULT 0,`next_attempt_at` datetime,`lease_until` datetime,`last_error` text);
CREATE INDEX IF NOT EXISTS `idx_gc_jobs_state_next` ON `gc_jobs`(`state`,`object_key`,`next_attempt_at`);

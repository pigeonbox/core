-- 回滚：去掉对象 GC 任务表（回到「删库即等全量对账」的无任务面形态）。
DROP TABLE IF EXISTS `gc_jobs`;

-- 观看保号只按用户权益本轮实际生效/接替时间起算。
-- 删除 plan_groups.watch_retention_reset_at，并替换分组校验约束；可重复执行。
-- 不回填或改写 user_entitlements 起点、失效状态及历史考核审计。
-- 起点为空的历史权益须核对真实记录后单独处理，不以分组/注册/迁移时间推算。
-- 删除列后旧版 API 不可直接回滚；部署时先停止旧 API/Gateway，再升级同版镜像。
ALTER TABLE plan_groups DROP CONSTRAINT IF EXISTS plan_groups_watch_retention_valid;
ALTER TABLE plan_groups DROP COLUMN IF EXISTS watch_retention_reset_at;
ALTER TABLE plan_groups ADD CONSTRAINT plan_groups_watch_retention_valid CHECK (
    watch_retention_days BETWEEN 1 AND 3650
    AND watch_retention_min_minutes BETWEEN 0 AND 5256000
    AND (NOT watch_retention_enabled OR watch_retention_min_minutes > 0)
);

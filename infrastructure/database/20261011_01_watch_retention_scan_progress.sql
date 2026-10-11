-- 观看保号扫描进度：每个用户仅保留最近一次尝试时间，独立于观看判定审计。
-- 可重复执行；无历史回填，尚无记录的候选用户优先处理。
-- 不修改权益、考核起点或失效状态；删除用户时级联清理进度。
CREATE TABLE IF NOT EXISTS watch_retention_scan_states (
    user_id varchar(25) PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    last_attempt_at timestamptz NOT NULL
);

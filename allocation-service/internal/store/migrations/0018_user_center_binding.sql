-- 统一用户中心绑定：带有效绑定票据的兑换，在扣码同一事务里写入兑换事件，
-- 并把票据 nonce 记入防重放表。匿名兑换不写这两张表。
-- 全部 IF NOT EXISTS：回滚到旧镜像时只删 schema_migrations 里的 18 号、保留这两张表，再次升级时原样接上
-- （事件 id 不回退，用户中心的拉取游标不会漏读）。
CREATE TABLE IF NOT EXISTS used_bind_nonces (
    nonce TEXT PRIMARY KEY,
    exp INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS used_bind_nonces_exp_idx ON used_bind_nonces(exp);

-- entitlement_ref 指向客户自己的卡（"card:<id>"），不是共享出去的账号 ID；
-- detail 只放非敏感摘要，不放密码 / 令牌 / 明文卡密。
CREATE TABLE IF NOT EXISTS redemption_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    event_uid TEXT NOT NULL UNIQUE,
    kind TEXT NOT NULL CHECK (kind IN ('new', 'renew')),
    code_hash TEXT NOT NULL,
    entitlement_ref TEXT NOT NULL,
    user_uid TEXT NOT NULL,
    succeeded_at TEXT NOT NULL,
    detail TEXT NOT NULL DEFAULT '{}'
);

CREATE INDEX IF NOT EXISTS redemption_events_entitlement_idx ON redemption_events(entitlement_ref);

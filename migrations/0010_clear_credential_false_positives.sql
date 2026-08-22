-- 凭据失效不是封禁证据。
-- token_revoked / credential_revoked / refresh_token_reused 说明存的 OAuth 凭据
-- 不能用了（在别处登录、改密码、token 轮换都会导致），账号本身可能完好无损。
-- 旧的 isAccountDenial 把这类失败计入"连续账号级拒绝"，于是三个正常账号被标成
-- 疑似封禁，其中两个还在对着一个永远刷不回来的 token 无限重试。
-- 代码侧已经改成只认 account_disabled，这里清理它留下的存量。

-- 1) 先通知分配域解除 suspected，再清标记 —— 顺序反了就选不出该通知谁。
--    只通知分配域已经认识的账号（发过 outbox 的），避免凭空建出一个空凭据账号。
--    event_id 用确定式拼接而不是 randomblob：后者每次引用都重新求值，
--    会让 outbox 主键和 payload 里的 event_id 对不上，分配域的幂等键就失准了。
UPDATE accounts SET sync_version = sync_version + 1
WHERE deleted_at IS NULL AND suspected_banned_at IS NOT NULL
  AND EXISTS (SELECT 1 FROM allocation_account_outbox o WHERE o.account_id = accounts.id);

INSERT INTO allocation_account_outbox
    (event_id, account_id, account_version, event_type, payload_json, delivery_status, next_attempt_at, created_at, updated_at)
SELECT e.event_id, e.id, e.sync_version, 'account.updated',
    json_object(
        'event_id', e.event_id,
        'version', e.sync_version,
        'event_type', 'account.updated',
        'occurred_at', e.stamp,
        'provider_account_id', e.provider_account_id,
        'email', e.email,
        'plan', e.plan,
        'subscription_expiry', e.expiry,
        'status', e.status
    ),
    'pending', e.stamp, e.stamp, e.stamp
FROM (
    SELECT 'migration-0010-' || a.id || '-' || a.sync_version AS event_id,
           strftime('%Y-%m-%dT%H:%M:%fZ', 'now') AS stamp,
           a.id, a.sync_version, a.provider_account_id,
           COALESCE(a.email, '') AS email, a.plan,
           COALESCE(a.current_expiry, a.auth_expiry) AS expiry, a.status
    FROM accounts a
    WHERE a.deleted_at IS NULL AND a.suspected_banned_at IS NOT NULL
      AND EXISTS (SELECT 1 FROM allocation_account_outbox o WHERE o.account_id = a.id)
) e;

-- 2) 收回全部误报。新规则下只有 account_disabled 计数，而这类失败当场就会终结或
--    暂停账号，攒不出连续 3 次，所以现存的 suspected 标记必然全是误报。
UPDATE accounts
SET denial_streak = 0,
    denial_streak_started_at = NULL,
    suspected_banned_at = NULL,
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE deleted_at IS NULL AND suspected_banned_at IS NOT NULL;

-- 3) 让凭据已失效的账号停在"需要重新授权"，而不是继续空转重试。
--    错误码沿用 finalizeLegacyPendingBans 的归一方式，保持管理端展示一致。
UPDATE accounts
SET last_check_state = 'reauthorization_required',
    last_check_error_code = CASE WHEN last_check_error_code = 'refresh_token_reused'
        THEN 'oauth_refresh_token_reused' ELSE 'oauth_refresh_token_invalid' END,
    polling_paused = 1,
    pause_reason = 'reauthorization_required',
    next_retry_at = NULL,
    pending_evidence_signature = NULL,
    pending_detected_at = NULL,
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE deleted_at IS NULL AND status = 'alive' AND polling_paused = 0
  AND last_check_state = 'error'
  AND last_check_error_code IN ('token_revoked', 'credential_revoked', 'refresh_token_reused');

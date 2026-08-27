UPDATE chatgpt_accounts
SET status = CASE
    WHEN current_allocations >= max_concurrent_users THEN 'full'
    ELSE 'available'
END,
updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE status = 'pending_credentials'
  AND account_expiry > strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
  AND monitor_status NOT IN ('dead_normal', 'dead_banned')
  AND length(display_password_secret) > 0
  AND (length(display_2fa_secret) > 0 OR length(pickup_address_secret) > 0);

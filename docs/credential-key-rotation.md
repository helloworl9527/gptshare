# 凭证主密钥轮换

凭证密钥必须是相互独立的 32 字节随机值，并由仅管理员可读的 0600 EnvironmentFile 提供。轮换期间不要删除旧密钥。

1. 生成新密钥并以新 `key_id` 追加到 `CREDENTIAL_MASTER_KEYS`，保留所有旧条目。
2. 把 `CREDENTIAL_ACTIVE_KEY_ID` 切换为新 `key_id`，先在维护窗口停止服务进程。
3. 使用与服务相同的受控配置运行 `server reencrypt-credentials`。命令在单个事务中解密所有账号凭证和未完成设备授权会话的旧信封，并使用新 active key、全新 nonce 和原 AAD 重加密；任一信封无法认证时整批回滚并返回非零退出码。
4. 用只返回计数的检查确认没有仍使用旧密钥的活动账号：

   ```sql
   SELECT count(*) FROM accounts
   WHERE deleted_at IS NULL AND length(enc_credentials) > 0
     AND credential_key_id <> '<new-key-id>';
   ```

   结果必须为 `0`。不要输出 `enc_credentials`，也不要对数据库做包含密文全文的 dump。
   同时确认 `device_auth_sessions` 中 `state='pending' AND length(enc_device_code)>0 AND credential_key_id<>'<new-key-id>'` 的计数为 `0`。
5. 重启服务并验证登录、账号列表和详情。经过备份保留期后，才可从 EnvironmentFile 撤下旧密钥。

失败或回滚时保留新旧密钥并恢复先前的 `CREDENTIAL_ACTIVE_KEY_ID`。因为批处理事务要么全部提交、要么全部回滚，旧 key ring 仍能读取提交前的数据；已经以新密钥提交的数据则要求新密钥继续可用。切勿在确认数据库中无对应 `key_id` 前删除任何密钥。

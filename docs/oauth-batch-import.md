# OAuth 手动回调与批量令牌导入安全边界

## 协议兼容性

OAuth 行为参考本仓库旁路审阅的 sub2api OpenAI OAuth 实现（Codex CLI public client，审阅日期 2026-07-26）独立实现，未引入其后端模块。兼容参数固定为：

- client ID `app_EMoamEEZ73f0CkXaXp7hrann`
- redirect URI `http://localhost:1455/auth/callback`
- scope `openid profile email offline_access`
- PKCE `S256`
- `id_token_add_organizations=true`
- `codex_cli_simplified_flow=true`
- `originator=codex_cli_rs`（与 2026-07-27 审阅的 OpenAI Codex CLI 授权请求保持兼容）

上游协议变化必须通过 `internal/chatgpt` 的契约测试评估后升级；不得在日志中加入授权码、PKCE verifier、callback URL 或 token 来排障。

## OAuth 会话

`oauth_auth_sessions` 仅保存加密 envelope、密钥 ID、目标账号、生命周期状态与时间戳。state、PKCE verifier 和标签位于 envelope 内，AAD 绑定 session ID。会话 15 分钟过期；兑换前以条件更新从 `pending` 原子切换到 `exchanging`，结束后清空 envelope 和密钥 ID。回调仅接受精确的 `http://localhost:1455/auth/callback`，state 使用常量时间比较。

三个 OAuth 接口与其他管理员写接口一样强制 Session、同源和 CSRF。callback 请求上限 8 KiB。state 只作为授权 URL 的标准查询参数出现，不另设响应字段；授权 URL 不包含 verifier，任何响应都不返回授权码或 token。

## 批量导入

批量接口只接受结构化 `items`，请求上限 1 MiB、最多 50 项。每项必须且只能包含 `access_token`、`refresh_token`、`session_token` 中的一项，字段与长度沿用单项导入规则。后端最多运行三个 worker，每项独立完成在线验证和事务写入；一个失败不会回滚其他成功项。

响应只包含零基输入序号、公开状态/错误码和成功账号摘要，不包含输入凭证或上游响应。前端提交前把逐行或 JSON 文本规范化，发起请求后立即清空原始文本；重试集合只保留于当前 Vue 组件内存，不写 localStorage、sessionStorage、IndexedDB 或 URL。

# 净化 fixture 说明

- `account_check_plus.json` 是 2026-07-22 实测响应的**结构化重建**；对应原始响应只保留 SHA-256 `482ef1c1b06e059a91a07dfd7c55b5522a035a4787bee603a098e7f9499a30c2`，fixture 中的账号、时间均为占位值。
- `error_*.json` 是用于验证响应分类、候选封号与保态边界的合成契约样本，证据等级只能是 `contract_verified_live_pending`，不代表已取得真实封禁、吊销、权限、限流或暂态响应证据。
- 未被本目录 fixture/合成测试覆盖的未知错误签名必须保持 `unverified`、`BannedCandidate=false` 并 fail closed。
- 本目录禁止出现 token、Cookie、邮箱、原始账号 ID、设备码或原始上游响应体。

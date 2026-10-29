# 系统 API

根路径 `/api/v1`；传 JSON 时必须使用 `Content-Type: application/json`。受保护接口使用唯一的 `Authorization: Bearer <token>` 请求头，不接受 cookie 或 URL 参数中的 token。

成功响应示例：

```json
{"data":{"ready":true},"requestId":"uuid","timeStamp":1790000000000}
```

错误通过 HTTP 状态和稳定错误码表达，消息不包含原始输入：

```json
{"error":{"code":"unauthorized","message":"请重新登录"},"requestId":"uuid","timeStamp":1790000000000}
```

`timeStamp` 为 Unix 毫秒。`X-Request-ID` 响应头与 `requestId` 对应，可关联访问日志。用户 ID 是 JSON 字符串，避免 JavaScript 大整数精度丢失。

## 接口表

| 方法 | 路径 | 权限 | 用途 |
|---|---|---|---|
| GET | `/healthz`（无 API 前缀） | 公开 | 进程存活 |
| GET | `/readyz`（无 API 前缀） | 公开 | 数据库连通 |
| POST | `/auth/login` | 公开、限流 | 登录 |
| GET | `/auth/me` | 登录 | 当前身份、角色和权限 |
| POST | `/auth/logout` | 登录 | 撤销当前会话 |
| PUT | `/auth/password` | 登录 | 修改本人密码，撤销所有旧会话 |
| GET | `/system/users` | `system:users:read` | 分页账号列表 |
| POST | `/system/users` | `system:users:write` | 创建账号 |
| PUT | `/system/users/:id/roles` | `system:users:write` | 替换角色集合 |
| PUT | `/system/users/:id/enabled` | `system:users:write` | 启停账号 |
| GET | `/system/roles` | `system:roles:read` | 角色及权限 |
| PUT | `/system/roles/:name` | `system:roles:write` | 创建或替换角色权限 |
| GET | `/system/settings` | `system:settings:read` | 已注册的配置列表 |
| GET | `/system/settings/:key` | `system:settings:read` | 单项配置 |
| PUT | `/system/settings/:key` | `system:settings:write` | 按 revision 更新 |
| GET | `/system/setting-audits` | `system:settings:audit` | 分页配置审计 |

`users` 和 `setting-audits` 支持 `?limit=20&offset=0`，limit 1–100，offset 0–1000000。响应 data 为 `{items,total,limit,offset}`。其他列表响应 data 为 `{items}`。

## 登录和账号

POST `/auth/login`：

```json
{"username":"admin","password":"填写私有配置中生成的初始密码"}
```

成功 data 包含 `token`、`token_type: "Bearer"`、`expires_in`（秒）和 `user`。此 token 是不透明随机字符串，前端无需解码。保存时按你的前端威胁模型选择存储方式，勿写进 URL。有效期由 `auth.session_ttl` 固定，不随请求自动续期；并行会话受 `auth.max_sessions` 限制，达到上限后新登录会撤销一个现有会话。进程重启后需要重新登录。

PUT `/auth/password` 使用 `{ "old_password": "...", "new_password": "..." }`。密码按 UTF-8 **字节**长度限制为 12–72。成功后重新登录。

POST `/system/users` 使用 `{ "username": "reader", "password": "至少十二字节的密码", "roles": ["viewer"] }`。角色须先创建；账号名/角色名为 3–64 位 ASCII 字母、数字、下划线、点或短横线，首位必须为字母/数字。

PUT `/system/users/:id/roles` 使用 `{ "roles": ["viewer"] }`，最多 32 个。空集合清除角色。PUT `/system/users/:id/enabled` 必须提供 `{ "enabled": false }`。

PUT `/system/roles/viewer` 使用 `{ "permissions": ["system:settings:read"] }`，最多 100 项。完整替换权限集合，空集合取消全部权限。`admin` 是受保护的内置角色，权限固定为 `*`；不能禁用或移除最后一个有效管理员的 admin 角色。

## 配置更新与冲突

GET `/system/settings/system.display_name` 的 data 示例：

```json
{"key":"system.display_name","description":"系统显示名称","value":"heaGo-scaffold","secret":false,"configured":true,"revision":1,"updated_at":"2026-10-26T00:00:00Z"}
```

PUT 同一路径：

```json
{"value":"我的服务","revision":1}
```

成功后 revision 递增。两人同时基于 revision 1 保存时，只有一人成功，另一人收到 **409 `revision_conflict`**；重新读取并让用户处理差异。不要无条件自动重试覆盖。

secret 项不含 value；`configured` 只说明是否配置。写入密钥同样提供 value/revision。`{"value":null,"revision":2}` 明确清空，`{"revision":2}` 被拒绝。未注册 key 返回 404，普通项变更 JSON 类型返回 400。

审计只记录 key、actor（认证账号 ID）、前后 revision 和时间，不保存前后值或密文。当前审计覆盖配置修改；账号/角色修改尚未提供独立审计 API。

## 常见状态

| 状态 | 含义 |
|---|---|
| 400 | JSON/字段/类型/参数不合法 |
| 401 | 凭据错误、会话过期、账号禁用或改密后旧会话 |
| 403 | 角色权限不足、管理员保护规则、CORS 来源拒绝 |
| 404 / 405 | 接口/资源不存在、方法不支持 |
| 409 | 账号重复或配置版本冲突 |
| 413 / 415 | 请求体过大、Content-Type 不支持 |
| 429 | 登录尝试超过当前进程每分钟预算 |
| 500 / 503 | 未处理异常、数据库/服务不可用 |

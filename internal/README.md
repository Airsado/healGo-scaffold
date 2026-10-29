## 职责

保存当前应用内部的基础能力和持久化模型。Go 的 internal 导入规则限制外部工程直接依赖；这是可复制的应用模板，业务仍在同一 Go module 内开发。

## 内容

[auth](auth/README.md) 管账号、会话、授权；[db](db/README.md) 管 PostgreSQL/MySQL 及显式主副本读取；[model](model/README.md) 集中登记模型；[logger](logger/README.md) 管日志；[middleware](middleware/README.md) 管 HTTP 边界；[response](response/README.md) 管响应；[settings](settings/README.md) 持久化配置；[stream](stream/README.md) 管 WebSocket/SSE。

业务 HTTP、用例和查询分别放在 [pkg/serve](../pkg/serve/README.md)，公开响应在 [pkg/vo](../pkg/vo/README.md)，路径与权限在 [pkg/router/routes](../pkg/router/routes/README.md)。

## 约定

模型不执行查询，基础包不反向引用 controller 或业务实现。连接由组合根创建和关闭；所有请求 I/O 传递 ctx，不使用全局 DB 或服务定位器。测试入口为 `go test ./internal/...`；数据库测试分别设置 `HEAGO_TEST_DATABASE_URL`（PostgreSQL）或 `HEAGO_TEST_MYSQL_DSN`（MySQL），只能指向专用测试实例。

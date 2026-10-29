## 职责

把已完成初始化的 Container 转成 Gin Engine，组合中间件、健康检查和资源路由；不打开数据库、不重新创建服务、不在首次请求时初始化表。

## 内容

- [router.go](router.go)：`New(*wire.Container) (*gin.Engine, error)`，建立 `/api/v1` 公开和登录分组，注册存活/数据库就绪检查及统一 404/405。
- [routes](routes/README.md)：system、note、realtime 的路径、方法、权限声明。
- [router_test.go](router_test.go)：系统 API、权限、配置和 HTTP 边界；资源集成测试靠近 routes。

## 约定

调用顺序为 Container → router.New → Runtime。仅 EnableExamples 开启时注册示例路由；通用入口默认关闭。路由层不写 JSON 绑定、业务规则和 SQL。请求 ID 先于日志/错误响应；panic、正文限制、CORS 和普通请求超时复用统一中间件。

在根目录运行 `go test ./pkg/router/...`；真实数据库场景使用 `HEAGO_TEST_DATABASE_URL`（PostgreSQL）或 `HEAGO_TEST_MYSQL_DSN`（MySQL），两个驱动分开验收。具体开发流程见 [手册](../../docs/DEVELOPMENT.md)。

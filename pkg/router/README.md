## 职责

采用 gin-scaffold 的“先准备引擎，再集中登记模块”写法。启动阶段传入已完成初始化的 Container；路由层不打开数据库、不重新创建服务、不在首次请求时初始化表。

## 内容

- [engine.go](engine.go)：`NewEngine(*wire.Container) (*gin.Engine, error)`，创建 Gin Engine，设置可信代理、全局中间件、存活/数据库就绪检查及统一 404/405。
- [router.go](router.go)：`New(*gin.Engine, *wire.Container) error`，检查模块依赖，建立 `/api/v1` 公开和登录分组，逐一显式调用模块登记函数。
- [routes](routes/README.md)：system、note、realtime 的路径、方法、权限声明。
- [router_test.go](router_test.go)：系统 API、权限、配置和 HTTP 边界；资源集成测试靠近 routes。
- [engine_test.go](engine_test.go)：无需数据库的分步登记、依赖校验、登录限流和重复登记脱敏回归。

启动调用顺序如下，实际入口见 [cmd/gin_server.go](../../cmd/gin_server.go)：

```go
engine, err := router.NewEngine(container)
if err != nil {
    return err
}
if err := router.New(engine, container); err != nil {
    return err
}
```

`NewEngine` 成功时只挂载基础 HTTP 行为，业务模块由第二步 `New` 登记。中央入口保持普通函数调用，新增模块在这里增加一行即可：

```go
public := engine.Group("/api/v1")
protected := engine.Group("/api/v1", middleware.Authenticate(container.Auth))
routes.RegisterSystemRoutes(container, public, protected)
if container.EnableExamples {
    routes.RegisterNoteRoutes(container, protected)
    routes.RegisterRealtimeRoutes(container, public, protected)
}
```

## 约定

调用顺序为 Container → router.NewEngine → router.New → Runtime。两步均须检查错误；重复或冲突路径转换为脱敏启动错误，失败的引擎不能继续监听，已打开资源仍由组合根关闭。仅 EnableExamples 开启时注册示例路由；通用入口默认关闭。

Container 只在启动与路由层取已注入对象，Controller/Service 继续接收各自明确的依赖。模块使用原生 Gin Group/GET/POST 等 API，不引入自动扫描、全局容器或额外注册接口。全局 HTTP 行为改 engine.go，模块挂载改 router.go，资源路径和操作权限改 routes 下对应文件。

请求 ID 先于日志/错误响应，CORS 先于认证，panic、正文限制和普通请求超时复用统一中间件。WS/SSE 的连接认证与取消仍由现有控制器和流组件处理。

在根目录运行 `go test ./pkg/router/...`；真实数据库场景使用 `HEAGO_TEST_DATABASE_URL`（PostgreSQL）或 `HEAGO_TEST_MYSQL_DSN`（MySQL），两个驱动分开验收。具体开发流程见 [手册](../../docs/DEVELOPMENT.md)。

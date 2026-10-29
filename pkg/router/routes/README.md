# 资源路由

## 职责

集中声明每个资源的 URL、HTTP 方法、操作权限和控制器方法。全局中间件由父级 router 统一配置，routes 不创建对象、不绑定请求、不查询数据库。

## 内容

- [system_routes.go](system_routes.go)：RegisterSystemRoutes，公开登录加限流，其余登录或具体管理权限。
- [note_routes.go](note_routes.go)：RegisterNoteRoutes，声明读写权限和五个 CRUD 操作。
- [realtime_routes.go](realtime_routes.go)：RegisterRealtimeRoutes，SSE/ticket 走登录组；WS 在升级前由控制器认证。
- [note_routes_test.go](note_routes_test.go)、[realtime_routes_test.go](realtime_routes_test.go)：真实 HTTP、权限、归属、版本及流式边界。

笔记路由的实际关键摘录如下；protected 已完成 `/api/v1` 登录校验，常量、完整函数和 imports 见 note_routes.go：

```go
group := protected.Group("/examples/notes")
read := middleware.Permission(authentication, NoteReadPermission)
write := middleware.Permission(authentication, NoteWritePermission)
```

```go
group.POST("", write, notes.Create)
group.GET("", read, notes.List)
group.GET("/:id", read, notes.Get)
group.PUT("/:id", write, notes.Update)
group.DELETE("/:id", write, notes.Delete)
```

## 约定

新增资源使用 `*_routes.go` 与 `Register<Resource>Routes`，再由 [router.go](../router.go)显式调用。真实业务路由登记在正式路径，教学路由由 EnableExamples 开关控制；避免与 auth/system 或其他资源冲突。

有登录不代表有操作权限；有读写权限不代表能读写其他 owner 的数据。权限由中间件检查，归属必须继续传入 service 与 SQL。WS 的 public 组只表示不经过普通登录组，控制器仍在升级前认证，不能误当匿名接口。

根目录运行 `go test ./pkg/router/routes -count=1`，真实 HTTP 集成使用 HEAGO_TEST_DATABASE_URL（PostgreSQL）或 HEAGO_TEST_MYSQL_DSN（MySQL），分别验收；参考 [开发手册](../../../docs/DEVELOPMENT.md)手动演示。

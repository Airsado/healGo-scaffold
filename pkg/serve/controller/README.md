# HTTP 控制器

## 职责

将 HTTP 参数与认证身份转换成用例调用，再把稳定结果转成 VO、错误转成状态码。controller 不执行 GORM、不实现业务规则、不管理共享连接生命周期。

## 内容

| 文件 | 内容 |
|---|---|
| [system_controller.go](system_controller.go) | 登录/退出/改密、账号、角色、持久化配置与审计；NewSystemController 注入 Auth、Settings、会话秒数 |
| [note_controller.go](note_controller.go) | NewNoteController(service.NoteService)、完整 CRUD、身份/ID/query 解析与错误映射 |
| [dto/note_dto.go](dto/note_dto.go) | CreateNoteRequest、UpdateNoteRequest，只声明可写请求字段 |
| [realtime_controller.go](realtime_controller.go) | NewRealtimeController(Auth,Streams,Origins)、SSE tick、WS echo 与周期鉴权 |
| [realtime_tickets.go](realtime_tickets.go) | 30 秒、单次、最多 128 条的进程内浏览器握手凭据 |
| [realtime_tickets_test.go](realtime_tickets_test.go) | 单次消费、到期、容量与并发 |

### 创建请求的实际写法

以下是 note_controller.go 的关键摘录，完整 imports、ownerID 和 fail 函数见源码：

```go
type NoteController struct{ service service.NoteService }

func NewNoteController(notes service.NoteService) *NoteController {
    return &NoteController{service: notes}
}
```

```go
owner, ok := ownerID(c)
if !ok {
    return
}
var input dto.CreateNoteRequest
```

```go
if !response.BindJSON(c, &input) {
    return
}
note, err := h.service.Create(c.Request.Context(), owner, service.CreateNoteInput{
    Slug: input.Slug, Title: input.Title, Content: input.Content,
})
if err != nil {
    fail(c, err)
    return
}
response.Write(c, http.StatusCreated, vo.FromNoteModel(note))
```

ownerID 从 middleware.Principal 取得身份；请求 DTO 中不接受 owner。BindJSON 处理格式/必填/未知字段，impl 处理 slug、标题和正文的业务规则。传入请求 ctx，取消能继续到数据库；返回 VO 不直接暴露 model。

### 新增控制器步骤

1. 在对应 DTO 定义允许输入的字段，确定路径/query 的严格解析规则；不要绑定数据库实体。
2. 为资源新增 `*_controller.go`，构造函数接收 service 接口；按身份 → 绑定 → 调用 → 分类错误 → VO 响应组织 handler。
3. 在 `pkg/router/routes` 声明路径与权限，再由 Container 和路由总入口接入。
4. 测未登录/无权限、非法字段、资源归属、错误码与公开字段，完整示例见 [开发手册](../../../docs/DEVELOPMENT.md)。

## 约定

note 的稳定错误通过 errors.Is 映射为 400/404/409/503；401/403 由身份和操作权限边界负责，不解析 err.Error 或回显驱动错误。PUT 用 DTO 指针区分缺字段和主动清空。

realtime 不持有 Gin Context 供后台使用；回调只保存独立 ctx 与协议对象。每次授权复核先 Authenticate 再 RequirePermission。流首帧/WS 升级之后只关闭或返回，不能追加 JSON envelope；WS 持续读取以驱动控制帧。浏览器先申请 ticket，不把普通 token 放 URL。

在根目录运行 `go test ./pkg/serve/controller ./pkg/router/routes -count=1`；HTTP 集成使用 HEAGO_TEST_DATABASE_URL（PostgreSQL）或 HEAGO_TEST_MYSQL_DSN（MySQL），分别运行。构造函数、路径及各层边界见 [分层示例](../../../docs/LAYER_EXAMPLES.md)，流协议见 [STREAMING](../../../docs/STREAMING.md)。Controller 不暴露主副本选择，连接与一致性规则见 [数据库手册](../../../docs/DATABASE.md)。

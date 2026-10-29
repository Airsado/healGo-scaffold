# Controller、Service、Mapper 规范与真实示例

本模板按固定 MVC 目录开发，新增资源时分别增加 `*_controller.go`、`*_service.go`、`*_service_impl.go`、`*_mapper.go`、`*_routes.go`、`*_dto.go`、`*_vo.go`。下面用已经参与编译和测试的笔记 CRUD 串起一个创建请求；代码块是实际源码摘录，完整 imports、辅助函数和其他操作请打开对应文件。

## 1. 从这些文件开始

| 层 | 规范 | 完整源码 |
|---|---|---|
| routes | [路由](../pkg/router/routes/README.md) | [note_routes.go](../pkg/router/routes/note_routes.go) |
| controller | [HTTP 控制器](../pkg/serve/controller/README.md) | [note_controller.go](../pkg/serve/controller/note_controller.go) |
| 请求 DTO | [输入字段](../pkg/serve/controller/dto/README.md) | [note_dto.go](../pkg/serve/controller/dto/note_dto.go) |
| service | [用例契约](../pkg/serve/service/README.md) | [note_service.go](../pkg/serve/service/note_service.go) |
| service/impl | [规则实现](../pkg/serve/service/impl/README.md) | [note_service_impl.go](../pkg/serve/service/impl/note_service_impl.go) |
| mapper | [SQL 写法](../pkg/serve/mapper/README.md) | [note_mapper.go](../pkg/serve/mapper/note_mapper.go) |
| model | [表结构](../internal/model/note/README.md) | [note.go](../internal/model/note/note.go)、[集中登记](../internal/model/get_all_models.go) |
| VO | [响应字段](../pkg/vo/README.md) | [note_vo.go](../pkg/vo/note_vo.go) |
| 装配 | [Container 与 provider sets](../pkg/wire/README.md) | [manual.go](../pkg/wire/manual.go)、[providers.go](../pkg/wire/providers.go) |

`service` 声明接口和普通 Go 输入/结果，`service/impl` 实现业务；根 `main.go` 调用 `cmd.Run` 处理进程启动。`mapper` 处理数据访问，不再并列创建同义 repository/dao。

## 2. 一次创建请求怎样经过各层

```mermaid
flowchart LR
    R[routes：认证与权限] --> C[controller：HTTP 转换]
    C --> S[service.NoteService 契约]
    S --> I[impl：业务规则]
    I --> M[mapper：持久化]
    M --> D[(PostgreSQL/MySQL 约束)]
    C --> V[VO：公开响应]
```

向 `POST /api/v1/examples/notes` 提交：

```json
{"slug":"first-note","title":"  第一条笔记  ","content":"正文"}
```

请求须先通过 Bearer 和写权限。路由权限控制“能不能创建”，owner 条件控制“属于谁”；管理员的 `*` 权限也不会绕过数据归属。

### Controller：只把 HTTP 翻译成用例

以下摘自 [note_controller.go](../pkg/serve/controller/note_controller.go)。构造函数接收接口，不接收 DB、具体实现或整个 Container。

```go
type NoteController struct{ service service.NoteService }

func NewNoteController(notes service.NoteService) *NoteController {
    return &NoteController{service: notes}
}
```

Create 的关键代码摘录如下，辅助函数见完整文件：

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

身份来自认证 Principal，不能从正文的 `owner_id` 推导。`BindJSON` 拒绝未知字段；DTO 只声明可写字段。PUT 用指针区分“没有提交 content”和“明确提交空字符串”，不把缺失误当清空。controller 将稳定错误映射为 400/404/409/503，不回显原始错误。

### Service：契约与实现分开

[service/note_service.go](../pkg/serve/service/note_service.go) 声明实际用例接口：

```go
type NoteService interface {
    Create(context.Context, uint64, CreateNoteInput) (model.Note, error)
    Get(context.Context, uint64, uint64) (model.Note, error)
    List(context.Context, uint64, int, int) (NotePage, error)
    Update(context.Context, uint64, uint64, UpdateNoteInput) (model.Note, error)
    Delete(context.Context, uint64, uint64, uint64) error
}
```

同一文件声明使用方需要的 `NoteMapper` 窄接口及 `CreateNoteInput`、`UpdateNoteInput`、`NotePage`，不导入 Gin/GORM。实现放在 [service/impl/note_service_impl.go](../pkg/serve/service/impl/note_service_impl.go)：

```go
func NewNoteService(mapper service.NoteMapper) service.NoteService {
    return &NoteServiceImpl{mapper: mapper}
}
```

```go
func (s *NoteServiceImpl) Create(ctx context.Context, ownerID uint64, input service.CreateNoteInput) (model.Note, error) {
    // 标题允许正常排版空白，但持久化前规范首尾空白，让长度规则和展示结果一致
    input.Title = strings.TrimSpace(input.Title)
    // Slug 不自动改写：悄悄转小写或删字符会把用户输入合并成同一个唯一地址
    if !validID(ownerID) || len(input.Slug) < 1 || len(input.Slug) > 64 || !slugPattern.MatchString(input.Slug) || !validText(input.Title, input.Content) {
        return model.Note{}, service.ErrInvalidInput
    }
    if s == nil || s.mapper == nil {
        return model.Note{}, service.ErrUnavailable
    }
    return s.mapper.Create(ctx, model.Note{OwnerID: ownerID, Slug: input.Slug, Title: input.Title, Content: input.Content, Revision: 1})
}
```

`validID`、`validText`、`slugPattern` 也在完整源码中。标题去两端空白后为 1–120 字符，正文允许空、最多 10000 字符，拒绝 NUL/非法 UTF-8；slug 只允许小写字母、数字和词间连字符。这些规则对 HTTP、任务和命令行调用同样有效。service/impl 不读取 Gin Context，也不拼 SQL。

### Mapper：带 ctx、owner 和约束执行 SQL

以下摘自 [note_mapper.go](../pkg/serve/mapper/note_mapper.go)：

```go
func NewNoteMapper(database *gorm.DB) (*NoteMapper, error) {
    if database == nil {
        return nil, model.ErrInvalidInput
    }
    return &NoteMapper{database: database}, nil
}
```

```go
func (m *NoteMapper) Create(ctx context.Context, note model.Note) (model.Note, error) {
    // ID 与版本由持久化入口固定，调用者不能通过“创建”覆盖已有资源
    note.ID = 0
    note.Revision = 1
    if err := m.database.WithContext(ctx).Create(&note).Error; err != nil {
        return model.Note{}, safeError(ctx, err)
    }
    return note, nil
}
```

`WithContext` 向驱动传播取消与期限。Get/List/Update/Delete 的 SQL 都保留 owner 范围，不先读出他人记录再由 controller 过滤。模型的 `(owner_id, slug)` 唯一约束处理并发重复创建；`safeError` 将驱动错误转换为稳定类别，具体参数和 SQL 不跨层外泄。

更新用 map 明确写入空正文；owner、ID、revision 比较与 revision 自增在同一 SQL 完成。PostgreSQL 用 `RETURNING`，MySQL 在同事务内带锁回读本次修改的行；`RowsAffected == 0` 固定主库区分不可见/不存在与版本冲突。完整代码与解释见 [mapper 规范](../pkg/serve/mapper/README.md)。

### VO：只公开明确选择的字段

[vo.FromNoteModel](../pkg/vo/note_vo.go) 显式复制 ID、slug、标题、正文、revision 和时间，不公开 owner。ID 用 JSON 字符串避免浏览器大整数舍入，分页空 Items 保持 `[]`。请求 DTO、服务输入、表模型和响应 VO 有不同的职责，不能全部复用表实体。

## 3. 默认手写注入，可以直接看懂构造顺序

[manual.go](../pkg/wire/manual.go) 的真实构造片段：

```go
noteMapper, err := mapper.NewNoteMapperWithReader(cluster)
if err != nil { return nil, nil, err }
noteService := impl.NewNoteService(noteMapper)
noteController := controller.NewNoteController(noteService)
```

`NewContainer(ctx, cfg, opts)` 完成资源与对象构造，再统一 Prepare；构造函数不能查表、建表、启动后台任务。Container 已完成模型和种子后，CLI 调用 `router.New(container)`，再创建 Runtime。默认入口也可构造示例对象，但只有 `EnableExamples` 开启时才登记示例表与路由。

`cluster.Primary` 是普通主库连接。`NewNoteMapperWithReader` 仅让 List 显式通过 Cluster.Read，其他方法固定主库；没有副本时 List 也走主库。事务内使用上面的 `NewNoteMapper(tx)`，所有操作都绑定该 tx。新增资源如何选择一致性和连接见 [数据库手册](DATABASE.md)。

可选 `NewContainerWithWire` 复用相同构造函数与 Prepare；所有 provider sets 集中在 [providers.go](../pkg/wire/providers.go)：Infrastructure、Mapper、Service、Controller、All。MapperProviders 中的实际接口绑定为：

```go
wire.Bind(new(service.NoteMapper), new(*mapper.NoteMapper))
```

只有一张 Wire 图。默认 `-di manual`，`-di wire` 切换生成装配；新增依赖时同步两条路径，生成文件不用手改。

## 4. 新增资源时的最短顺序

1. 在 `internal/model/<资源>/` 定义表和约束，并在 `get_all_models.go` 登记。
2. 在 `controller/dto/<资源>_dto.go` 和 `pkg/vo/<资源>_vo.go` 明确允许输入和公开输出。
3. 在 `service/<资源>_service.go` 声明用例、输入/结果、所需 mapper 接口，在 `service/impl/<资源>_service_impl.go` 实现规则。
4. 在 `mapper/<资源>_mapper.go` 实现持久化；在 controller 适配身份、ctx、错误与 VO。
5. 在 `routes/<资源>_routes.go` 声明路径/权限；在 Container、manual.go、providers.go 和 router 总入口接上对象与路由。
6. 使用同样配置 migrate、serve；验证主流程、401/403、归属、非法输入、唯一约束和版本冲突。

事务共享 tx、具体接入点和可编辑配置见 [扩展手册](EXTENDING.md)。当前场景能演示、权限和数据边界通过后即可结束，不用先增加缓存或消息队列。

## 5. 运行与验证

先按 [开发手册](DEVELOPMENT.md#1-先把示例运行起来)准备数据库与私有 YAML（默认演示 PostgreSQL）：

```powershell
go run ./cmd/example migrate -config configs/config.local.yaml
go run ./cmd/example serve -config configs/config.local.yaml -di manual
# 停止后也可用 -di wire 启动，验证相同 HTTP 行为
```

```powershell
go test ./pkg/serve/service/impl ./internal/model/note -count=1
go generate ./pkg/wire
go tool wire diff ./pkg/wire
go test ./pkg/serve/mapper ./pkg/router/... ./pkg/wire -count=1
go build -o .build/heago-example ./cmd/example
```

impl/model 单元测试不需要数据库；mapper、HTTP 和 Container 集成测试使用 `HEAGO_TEST_DATABASE_URL` 或 `HEAGO_TEST_MYSQL_DSN`，两个驱动分别运行，未设置时的 skip 不能算真实数据库验证。完整登录、CRUD、409 和删除脚本见 [开发手册](DEVELOPMENT.md)。

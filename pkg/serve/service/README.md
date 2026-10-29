## 职责

声明业务用例契约、用例输入、结果和需要的最小存储接口。controller 依赖这里的接口，具体实现放在 [impl/](impl/README.md)；本目录不执行 HTTP 或数据库操作。

以创建笔记为例，HTTP 层处理 JSON 类型和认证身份，service 实现规范化并检查业务规则，mapper 负责 SQL 范围和并发约束。HTTP 校验成功不代表用例一定合法，因为相同用例也可能由命令行或任务调用。

| 边界 | 本例负责的规则 |
|---|---|
| controller / DTO | Content-Type、未知字段、JSON 类型、路径和查询参数解析 |
| service / impl | 标题修剪后非空、slug 格式、文本长度、owner 和 revision 范围 |
| mapper / model | owner SQL 范围、同 owner 的 slug 唯一索引、revision 条件写入 |

## 内容

| 文件或目录 | 阅读重点 |
|---|---|
| [note_service.go](note_service.go) | `NoteService`、`NoteMapper`、`CreateNoteInput`、`UpdateNoteInput`、`NotePage` 和稳定错误 |
| [impl/note_service_impl.go](impl/note_service_impl.go) | 规则的具体实现和构造器 `NewNoteService` |
| [impl/note_service_impl_test.go](impl/note_service_impl_test.go) | 内存替身验证非法输入不进入持久化、身份和 context 透传 |
| [持久化模型](../../../internal/model/note/note.go) | 表、索引、版本和跨层错误原值 |
| [GORM mapper](../mapper/note_mapper.go) | 存储接口的具体实现 |

下面是 [note_service.go](note_service.go) 中的实际接口摘录，省略 package、imports 和其他声明；不要把摘录当作独立完整文件：

```go
// 普通 context 让 HTTP、命令行和任务能共享相同用例
type NoteService interface {
    Create(context.Context, uint64, CreateNoteInput) (model.Note, error)
    Get(context.Context, uint64, uint64) (model.Note, error)
    List(context.Context, uint64, int, int) (NotePage, error)
    Update(context.Context, uint64, uint64, UpdateNoteInput) (model.Note, error)
    Delete(context.Context, uint64, uint64, uint64) error
}

// ownerID 不放在输入中，避免客户端覆盖已认证身份
type CreateNoteInput struct { Slug, Title, Content string }
type UpdateNoteInput struct {
    Title, Content string
    Revision uint64
}
```

`CreateNoteInput` 等用例类型不带 JSON 标签或 HTTP 绑定规则。输入和结果名称带 Note 前缀，便于多个业务共享固定的 service 包；不要在同包中为每个业务都定义 `Input` 或 `Page`。

`NoteMapper` 由使用它的 service 声明，mapper 提供实现，测试提供内存替身。这是调用方真正需要的五个操作，不是给未来业务预留的通用仓储接口。构造根通过 `mapper.NewNoteMapper(db)` 和 `impl.NewNoteService(store)` 直接装配，不依赖包初始化注册。

## 约定

新业务先声明一个核心用例和它实际需要的存储操作，再在 impl 编写实现。接口不导入 Gin、GORM 或具体 mapper，不从全局变量拿数据库或认证对象。调用链必须保留上游 `context.Context`，不能换成 `context.Background()`。

`ownerID` 是可信入口传来的已认证身份；检查它是否在合法数值范围内不能代替登录认证。mapper 仍必须把 owner 写入每次 SQL，防止内部入口绕过 controller 时破坏隔离。

错误与模型共用 sentinel，调用方使用 `errors.Is`。service 不决定 HTTP 状态码，不解析错误字符串，不拼接原始 SQL 或用户内容。返回的 `model.Note` 是内部结果，controller 仍须显式转为 VO。分页空集合统一为非 nil 切片。

涉及多次写入一起成功时，由用例明确事务边界，再把同一个 tx 绑定给所有参与的 mapper；不能在事务中继续调用绑定普通 DB 的旧实例。无需为当前单条 CAS 更新增加事务抽象，完整例子见 [扩展手册](../../../docs/EXTENDING.md)。

在模板根目录运行 `go test ./pkg/serve/service/... -count=1` 即可验证本层，不需要数据库。真实唯一约束、owner SQL 和并发 CAS 由 [mapper 测试](../mapper/README.md) 对 PostgreSQL/MySQL 分别验证，内存替身不能替代它们。用例是否允许复制延迟属于业务契约，具体分流由 Mapper 实现；Service 不直接操作 GORM 节点。

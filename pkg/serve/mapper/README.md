## 职责

将用例需要的数据操作转换成 GORM 查询。controller 不调用 GORM；[service](../service/note_service.go) 声明最小存储接口，本目录提供具体实现。

本层负责传递 context、把 owner 条件放进 SQL、使用数据库约束保障并发写入，并将驱动错误转换成安全类别。HTTP 状态、JSON DTO、登录认证、标题规则和建表不属于本层。

## 内容

| 文件 | 阅读重点 |
|---|---|
| [note_mapper.go](note_mapper.go) | `NoteMapper`、`NewNoteMapper`、CRUD、`safeError` 和 CAS 错误分类 |
| [note_mapper_test.go](note_mapper_test.go) | PostgreSQL/MySQL CRUD、owner 隔离、唯一键、清空正文、版本冲突、并发更新与取消 |
| [note_mapper_error_test.go](note_mapper_error_test.go) | 驱动错误脱敏和 PostgreSQL 唯一冲突分类 |
| [模型](../../../internal/model/note/note.go) | 表名、联合索引、版本和稳定错误 |
| [服务实现](../service/impl/note_service_impl.go) | 调用 SQL 之前的规则与身份范围校验 |

以下代码块均摘自 [note_mapper.go](note_mapper.go)，省略 package、imports 或其他方法。需要完整可编译文件时查看源码，不要把各片段分别当成新文件。

### 绑定连接，再执行 SQL

```go
// database 固定主库或 tx，reader 只供显式允许延迟的 List 使用
type NoteMapper struct {
    database *gorm.DB
    reader *db.Cluster
}

// 构造器不查询、不迁移，因此启动装配与表初始化可以明确排序
func NewNoteMapper(database *gorm.DB) (*NoteMapper, error) {
    if database == nil {
        return nil, model.ErrInvalidInput
    }
    return &NoteMapper{database: database}, nil
}

func (m *NoteMapper) Create(ctx context.Context, note model.Note) (model.Note, error) {
    // 创建不能通过传入已有 ID 或版本覆盖其他资源
    note.ID = 0
    note.Revision = 1
    if err := m.database.WithContext(ctx).Create(&note).Error; err != nil {
        return model.Note{}, safeError(ctx, err)
    }
    return note, nil
}
```

`NewNoteMapper` 只保存共享 DB 或事务；不连接第二个数据库，也不执行 `AutoMigrate`。service 校验 slug、标题和正文后构造模型，mapper 负责插入。GORM 成功后将生成的主键和时间写回模型。

默认组合根使用 `NewNoteMapperWithReader(cluster)`，构造时绑定 `database=cluster.Primary` 和 `reader=cluster`，只让 List 的整个 Count/Find 回调走 `Cluster.Read(ctx,fn)`。没有副本时同样使用主库。其他方法及冲突判断固定 database；事务内用 `NewNoteMapper(tx)`，reader 留空，List 也不逃出事务。配置与可编译查询示例见 [数据库手册](../../../docs/DATABASE.md)。

`WithContext(ctx)` 将上游取消与期限传给驱动，每次 Count、Find、Updates、Delete 都必须使用它。不能在内部改用 `context.Background()`，否则已经取消的请求仍可能占用数据库 I/O。

### 数据范围必须进入查询

```go
func (m *NoteMapper) Get(ctx context.Context, ownerID, id uint64) (model.Note, error) {
    var note model.Note
    // 占位符将数据与 SQL 结构分开，同时限定当前 owner 的可见范围
    err := m.database.WithContext(ctx).Where("owner_id = ? AND id = ?", ownerID, id).First(&note).Error
    if err != nil {
        return model.Note{}, safeError(ctx, err)
    }
    return note, nil
}
```

不要先按 ID 读取所有字段，再依靠 controller 检查归属。另一个 owner 读取同一个 ID 应当得到不存在；列表的 Count 和 Find 也必须使用同一 owner，防止 total 暴露其他账号的数据数量。

`?` 后面传值，不拼接用户输入。列名与排序表达式也只允许来自受控代码。本例列表固定 `id DESC`，同时限制 offset 和 limit；Count 与 Find 是两次读取，并发增删时 total 可以代表相邻时刻的快照。

### 唯一性由索引最终决定

模型为 `owner_id + slug` 定义联合唯一索引。同 owner 不允许重复，不同 owner 可以用同一个 slug。不能用“先查不存在再插入”替代索引，因为两个并发请求可能同时查到不存在。

`safeError` 将 `gorm.ErrRecordNotFound` 映射为 `ErrNotFound`，将 GORM 重复键和 PostgreSQL SQLState `23505` 映射为 `ErrConflict`。其他驱动错误一律变为 `ErrUnavailable`，原始 SQL、地址和正文不跨层返回。取消错误通过 `errors.Join` 同时保留不可用和 context 取消分类。

```go
func safeError(ctx context.Context, err error) error {
    // 调用者仍能 errors.Is(err, context.Canceled)，但看不到原始驱动文本
    if ctx.Err() != nil {
        return errors.Join(model.ErrUnavailable, ctx.Err())
    }
    if errors.Is(err, gorm.ErrRecordNotFound) {
        return model.ErrNotFound
    }
    if errors.Is(err, gorm.ErrDuplicatedKey) {
        return model.ErrConflict
    }
    // 未启用 GORM TranslateError 时仍识别 PostgreSQL 唯一键冲突
    var state interface{ SQLState() string }
    if errors.As(err, &state) && state.SQLState() == "23505" {
        return model.ErrConflict
    }
    return model.ErrUnavailable
}
```

上层使用 `errors.Is`，不解析错误文本，不把原始 err 拼进公开响应。`23505` 仅表示唯一约束冲突，不能把所有数据库故障都报告为重复键。

### 修改必须比较旧版本

```go
func (m *NoteMapper) Update(ctx context.Context, ownerID, id, revision uint64, title, content string) (model.Note, error) {
    if m.database.Dialector.Name() == "mysql" {
        return m.updateMySQL(ctx, ownerID, id, revision, title, content)
    }
    var updated model.Note
    // map 保留 content 的空字符串；struct Updates 默认忽略零值
    // 比较旧版本与加一在同一 SQL 中完成，避免并发请求相互覆盖
    result := m.database.WithContext(ctx).Model(&updated).Clauses(clause.Returning{}).
        Where("owner_id = ? AND id = ? AND revision = ?", ownerID, id, revision).
        Updates(map[string]any{"title": title, "content": content, "revision": gorm.Expr("revision + 1"), "updated_at": time.Now().UTC()})
    if result.Error != nil {
        return model.Note{}, safeError(ctx, result.Error)
    }
    if result.RowsAffected == 0 {
        return model.Note{}, m.missingOrConflict(ctx, ownerID, id)
    }
    // 返回本次写入结果，另发 Get 可能已读到其他请求写入的下一版
    return updated, nil
}
```

CAS 表示“旧版本仍匹配才修改”：客户端读取 revision 1，成功修改得到 revision 2，再提交 revision 1 应报告冲突。先 Get、比较内存变量、再不带 revision 更新无法保证这一点。

这里四个细节需要一起保留：允许修改的列由代码列出，`map` 明确保留空正文；owner、ID、revision 同时进入 WHERE；版本加一与比较在同一 SQL 内执行；结果返回本次修改的行。PostgreSQL 用 RETURNING，MySQL 的 `updateMySQL` 在同一事务中 UPDATE 后 `SELECT … FOR UPDATE`，确保回读的是当前版本并保持行锁到提交。外层已有事务时沿用 tx/savepoint，不新取连接。

`RowsAffected == 0` 不能当成功。`missingOrConflict` 固定主库在当前 owner 范围内补读：不可见或不存在返回 `ErrNotFound`，仍存在则返回 `ErrConflict`。MySQL 的更新及删除分类都使用锁定 current read，避免外层 REPEATABLE READ 事务的旧快照影响分类。补读只负责分类，本次写入是否成功已经由 CAS 决定。Delete 同样需要 owner、ID、revision 并检查影响行数。

## 约定

新增业务时先在 `internal/model/<name>` 定义结构、表名和索引，在 `internal/model/get_all_models.go` 显式登记；再在 service 声明所需存储接口，增加 `<name>_mapper.go` 和 `New<Name>Mapper(*gorm.DB)`。保持固定层级，不再叠加同义的 dao/repository。

构造根直接把 mapper 传给 service 实现，再把 service 接口传给 controller。每条 SQL 传 ctx、保留身份范围、固定可写列，明确空值、未找到、重复键和版本冲突的语义；测试通过后停止，不为未来需求添加查询生成器或缓存。

**事务传播。** CAS 的条件更新具备原子性，MySQL 内部再用事务保证返回结果。如果用例需要多次写入一起提交，从主库 `database.WithContext(ctx).Transaction(...)` 开始，用 `NewNoteMapper(tx)` 重新构造实例，再调用 `impl.NewNoteService(txMapper)`。所有参与者必须共用同一个 tx；调用旧 DB 或带 Cluster reader 的 Mapper 会逃出事务，回滚无法撤销它。完整事务与外层错误处理见 [扩展手册](../../../docs/EXTENDING.md)。

**测试入口。** 在模板根目录执行：

```powershell
# 不需要数据库：构造器无 I/O、驱动错误分类
go test ./pkg/serve/mapper -count=1 -run 'TestConstructorDoesNotQueryOrAcceptNil|TestDriverErrorsAreClassifiedWithoutLeakingDetails'
# 分别设置 HEAGO_TEST_DATABASE_URL 或 HEAGO_TEST_MYSQL_DSN 验证两种驱动
go test ./pkg/serve/mapper -count=1 -v
```

真实数据库测试创建并清理随机 schema/database。`HEAGO_TEST_DATABASE_URL` 和 `HEAGO_TEST_MYSQL_DSN` 都未设置时明确 skip；同时设置时只优先 PostgreSQL，因此必须分两次运行。不能把 skip 当成 SQL 已通过验证；配置方式见 [开发手册](../../../docs/DEVELOPMENT.md)。

| 现有测试 | 验证内容 |
|---|---|
| `TestCRUDOwnerIsolationAndRevision` | owner 隔离、联合唯一键、分页、空正文与旧版本冲突 |
| `TestConcurrentCASHasOneWinner` | 8 个请求提交同一 revision，只有 1 个成功 |
| `TestCanceledContextStopsAllPersistence` | 已取消操作不改数据，保留取消与不可用分类 |
| `TestConstructorDoesNotQueryOrAcceptNil` | nil 被拒绝，构造不查询 |
| `TestDriverErrorsAreClassifiedWithoutLeakingDetails` | 驱动信息不泄漏，SQLState 分类正确 |

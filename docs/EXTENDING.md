# 新增业务资源

先按 [开发手册](DEVELOPMENT.md)运行笔记 CRUD，再按同一套固定 MVC 目录新增自己的资源。这里使用 task 作为命名示意，模板没有预先增加 task 业务。

## 1. 实际接入点

| 接入点 | 现有文件 | 新资源增加什么 |
|---|---|---|
| 模型 | [internal/model/note/note.go](../internal/model/note/note.go) | internal/model/task/task.go：表、索引、约束、稳定错误 |
| 集中模型登记 | [get_all_models.go](../internal/model/get_all_models.go) | 真实业务模型加入正式模型列表；不要依赖示例开关 |
| 用例契约 | [note_service.go](../pkg/serve/service/note_service.go) | task_service.go：TaskService、输入/结果、TaskMapper 窄接口 |
| 实现 | [note_service_impl.go](../pkg/serve/service/impl/note_service_impl.go) | task_service_impl.go：规则与用例实现 |
| 查询 | [note_mapper.go](../pkg/serve/mapper/note_mapper.go) | task_mapper.go：构造、带 ctx/owner 的 SQL、稳定错误 |
| HTTP | [note_controller.go](../pkg/serve/controller/note_controller.go)、[note_dto.go](../pkg/serve/controller/dto/note_dto.go) | task_controller.go / task_dto.go |
| 响应 | [note_vo.go](../pkg/vo/note_vo.go) | task_vo.go：公开字段与显式转换 |
| 路由 | [note_routes.go](../pkg/router/routes/note_routes.go) | task_routes.go：方法、路径和权限，再由 router.go 调用 |
| 默认手写装配 | [manual.go](../pkg/wire/manual.go)、[container.go](../pkg/wire/container.go) | 构造 mapper → impl → controller，保存需要的 Container 字段 |
| 可选 Wire | [providers.go](../pkg/wire/providers.go)、[infrastructure.go](../pkg/wire/infrastructure.go) | 添加相应 provider、接口绑定与容器构造参数，重新生成 |

构造函数不查表、不迁移、不启动任务。只有 Container 的公共收尾 `finish` 调用 Prepare，统一模型准备和种子，之后 CLI 才调用 router.New。

## 2. 新增一条最小 CRUD

1. 先确定允许写入/公开的字段与归属规则。在 model 定义表名、唯一约束、版本等持久化结构，DTO 和 VO 分别定义请求与响应。
2. 在 service 包定义 `TaskService`、`CreateTaskInput`、`UpdateTaskInput`、分页结果，以及实际需要的最小 `TaskMapper` 接口；不导入 Gin/GORM。
3. 在 service/impl 实现规则，构造函数只绑定接口。参考真实签名 `impl.NewNoteService(service.NoteMapper) service.NoteService`，不从全局容器取服务。
4. 在 mapper 实现参数化 SQL。参考 `mapper.NewNoteMapper(*gorm.DB) (*mapper.NoteMapper, error)`，每次 I/O 传 ctx，读写都带可信 owner，修改/删除比较 revision。
5. 在 controller 注入服务接口，读取 Principal、严格绑定 DTO、传 ctx、分类错误并转 VO；不把表模型直接返回。
6. 在 routes 声明路径和权限，由 router 总入口注册；实际业务登记在正式路由中，不放在 EnableExamples 分支。
7. 接入下节的两条装配路径和集中模型列表。migrate 后 serve，验证当前 CRUD 主流程与权限/并发边界即可。

同一层的文件按资源命名，不把不同资源都写进一个 controller.go。复制笔记代码时替换 Note 类型、imports、example_notes 表名/索引、example:notes 权限和 examples/notes 路径；只在固定各层增加文件，不复制整个框架。

## 3. 默认手写注入与可选 Wire

默认路径为 `wire.NewContainer`。在 [manual.go](../pkg/wire/manual.go) 找到真实构造顺序：

```go
noteMapper, err := mapper.NewNoteMapperWithReader(cluster)
if err != nil { return nil, nil, err }
noteService := impl.NewNoteService(noteMapper)
noteController := controller.NewNoteController(noteService)
```

这是 buildManual 内的摘录，完整错误清理与返回值见源码。新增资源按相同顺序构造，再传给 newContainer 并保留需要供路由使用的字段。不要绕过现有的逆序 cleanup 或在中途重复打开共享 DB。

[providers.go](../pkg/wire/providers.go) 集中提供 InfrastructureProviders、MapperProviders、ServiceProviders、ControllerProviders、AllProviders。当前 MapperProviders 包含真实构造及绑定：

```go
var MapperProviders = wire.NewSet(mapper.NewNoteMapperWithReader,
    wire.Bind(new(service.NoteMapper), new(*mapper.NoteMapper)))
```

NewNoteService 已返回服务接口，controller 不依赖具体实现。新增资源时同步各层 provider 与容器参数，再生成唯一依赖图：

```powershell
go generate ./pkg/wire
go tool wire diff ./pkg/wire
go test ./pkg/wire ./pkg/router/... -count=1
```

Wire 声明有 wireinject 标签，普通源码不导入 Wire DSL；不手改 wire_gen.go。默认 `-di manual`，可在同一入口用 `-di wire` 验证等价装配。两条路径共用校验、Prepare 和关闭顺序，不能只修改其中一条。

CLI 构造 Container 后才调用 `router.New(container)`；wire 不 import router，避免包循环。`bootstrap.Options` 只包含 EnableExamples、InitializeSchema、Settings。`cmd/example` 的开关用于教学，真实业务应在正式模型/路由列表中显式启用。

## 4. 事务如何传播

笔记 CAS 的条件更新是一条原子 SQL；MySQL 为保证返回本次更新行，在 Mapper 内使用事务完成 UPDATE 和回读。业务中“多次写入必须一起成功”时，仍由应用用例定义外层事务边界，并把同一个 tx 注入所有相关 Mapper。

下面是可放在当前 Go module 的应用用例包中的完整示例，不会自动注册到任何路由。两次创建任何一次失败都会回滚；构造、输入类型均使用当前真实 API。

```go
package usecase

import (
    "context"
    "errors"

    "heago-scaffold/pkg/serve/mapper"
    "heago-scaffold/pkg/serve/service"
    "heago-scaffold/pkg/serve/service/impl"
    "gorm.io/gorm"
)

func CreatePair(ctx context.Context, database *gorm.DB, ownerID uint64,
    first, second service.CreateNoteInput) error {
    if database == nil {
        return service.ErrInvalidInput
    }
    err := database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
        txMapper, err := mapper.NewNoteMapper(tx)
        if err != nil {
            return err
        }
        notes := impl.NewNoteService(txMapper)
        if _, err := notes.Create(ctx, ownerID, first); err != nil {
            return err
        }
        _, err = notes.Create(ctx, ownerID, second)
        return err
    })
    if err == nil {
        return nil
    }
    // 开启事务或提交本身也可能返回驱动错误；对外只保留安全类别
    for _, known := range []error{service.ErrInvalidInput, service.ErrNotFound,
        service.ErrConflict, service.ErrUnavailable} {
        if errors.Is(err, known) {
            return known
        }
    }
    return service.ErrUnavailable
}
```

关键是 mapper.NewNoteMapper(tx)。传入的 database 必须为主库（由 Cluster.Primary 获取）。如果闭包内继续使用此前绑定普通 database 或 Cluster reader 的实例，就会逃出事务，后续回滚也无法撤销。多张表必须共享同一个 tx；impl 仍依赖接口，不反向导入具体 mapper/GORM。

用同 owner、相同 slug 创建两条笔记可验证回滚：第二次违反唯一约束，第一次也不应留下。mapper 的单方法测试不能替代整段事务用例测试。

默认 Mapper 使用主库。只有接受复制延迟的查询才引入 `Cluster.Read(ctx, fn)`；笔记 `NewNoteMapperWithReader(cluster)` 只让 List 使用该能力。认证、权限、配置、写后回读和冲突判断固定主库；回调只做安全可重试的读取。具体规则和代码见 [数据库手册](DATABASE.md)。

## 5. 接入持久化配置

数据库连接、加密主密钥和启动边界来自 YAML；可编辑配置通过 `internal/settings` 持久化。在应用 bootstrap.Options.Settings 注册 Definition，不能让 API 临时创建任意键：

```go
settings.Definition{
    Key: "feature.client_secret",
    Description: "第三方访问密钥",
    Default: json.RawMessage(`""`),
    Secret: true,
    Validate: func(value json.RawMessage) error {
        var secret string
        if json.Unmarshal(value, &secret) != nil || len(secret) > 4096 {
            return settings.ErrInvalidInput
        }
        return nil
    },
}
```

此处为已有入口中的配置定义片段，需正常导入 encoding/json 和 internal/settings。Default 只补缺失项，不覆盖数据库已有值。secret GET 不回显明文；`value:null` 明确清除。校验器应确定、允许预期的空状态、不访问网络或记录输入。

业务通过注入的 settings.Service.Value(ctx,key) 获取内部 JSON 后解析，不能把密钥直接返回前端。配置修改不会自动替换已经创建的 SDK 客户端，需要时由实际业务实现可验证的刷新策略。

## 6. 完成标准

能从路由走到 SQL 并返回 VO；未登录/无权限、owner 隔离、非法输入、唯一冲突、旧 revision 和并发覆盖均有验证。数据库约束/回滚用真实 PostgreSQL 和 MySQL 分别验收；接受延迟的读取补充主副本分流及故障行为测试。两种注入路径、Wire diff、适当测试与文档通过后结束，不顺手添加与当前用例无关的基础设施。

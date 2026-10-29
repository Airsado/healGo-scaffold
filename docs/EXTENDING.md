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
| 默认 Wire 装配 | [container.go](../pkg/wire/container.go)、[providers.go](../pkg/wire/providers.go)、[wire.go](../pkg/wire/wire.go) | 增加 Controller 字段、按层 provider 和接口绑定，再生成依赖图 |
| 可选手写装配 | [manual.go](../pkg/wire/manual.go) | 构造 mapper → impl → controller，写入 Container 字面量 |

构造函数不查表、不迁移、不启动任务。Container 的公共收尾 `finish` 调用 Prepare，统一模型准备和种子；之后 CLI 调用 `router.NewEngine(container)` 建立引擎，再调用 `router.New(engine, container)` 登记路由。

## 2. 新增一条最小 CRUD

1. 先确定允许写入/公开的字段与归属规则。在 model 定义表名、唯一约束、版本等持久化结构，DTO 和 VO 分别定义请求与响应。
2. 在 service 包定义 `TaskService`、`CreateTaskInput`、`UpdateTaskInput`、分页结果，以及实际需要的最小 `TaskMapper` 接口；不导入 Gin/GORM。
3. 在 service/impl 实现规则，构造函数只绑定接口。参考真实签名 `impl.NewNoteService(service.NoteMapper) service.NoteService`，不从全局容器取服务。
4. 在 mapper 实现参数化 SQL。参考 `mapper.NewNoteMapper(*gorm.DB) (*mapper.NoteMapper, error)`，每次 I/O 传 ctx，读写都带可信 owner，修改/删除比较 revision。
5. 在 controller 注入服务接口，读取 Principal、严格绑定 DTO、传 ctx、分类错误并转 VO；不把表模型直接返回。
6. 在 routes 声明路径和权限，由 router 总入口注册；实际业务登记在正式路由中，不放在 EnableExamples 分支。
7. 在 Container 增加 Controller 字段，接入下节的分层 provider、接口绑定和集中模型列表，再执行 `go generate ./pkg/wire`。需要手写路径时同步 manual.go。migrate 后 serve，验证当前 CRUD 主流程与权限/并发边界即可。

同一层的文件按资源命名，不把不同资源都写进一个 controller.go。复制笔记代码时替换 Note 类型、imports、example_notes 表名/索引、example:notes 权限和 examples/notes 路径；只在固定各层增加文件，不复制整个框架。

## 3. 默认 Wire 注入与可选手写装配

默认 `wire.NewContainer(ctx, cfg, opts)` 调用生成的 `buildWithWire`，CLI 默认 `-di wire`。新增资源通常修改三个地方：在 [Container](../pkg/wire/container.go) 增加 Controller 字段，在 [providers.go](../pkg/wire/providers.go) 的对应层登记构造函数和接口绑定，在路由模块与中央入口登记路径；持久化资源还要加入集中模型列表。

以下为当前 Container 的字段声明，放在已有 container.go，保留该文件的 imports 和生命周期方法：

```go
type Container struct {
    Config configs.Config
    bootstrap.Resources
    EnableExamples     bool
    NoteMapper         *mapper.NoteMapper
    NoteService        service.NoteService
    SystemController   *controller.SystemController
    NoteController     *controller.NoteController
    RealtimeController *controller.RealtimeController
    // 生命周期由 finish 和 Close 管理，不参与字段注入
    cleanup   func()    `wire:"-"`
    closeOnce sync.Once `wire:"-"`
}
```

Container 只在装配和路由层传递。`NoteController` 的构造函数仍接收 `service.NoteService`，Service 仍接收 `service.NoteMapper`；handler 不通过 Container 临时查找依赖。示例保留 Mapper/Service 字段供验证使用，新业务按实际装配和路由需要增加字段。

[providers.go](../pkg/wire/providers.go) 按 Infrastructure、Mapper、Service、Controller、All 分层组织。以下三个集合是当前笔记与系统控制器的真实登记方式，使用该文件已有的 Wire、mapper、service、impl、controller imports：

```go
var MapperProviders = wire.NewSet(
    mapper.NewNoteMapperWithReader,
    wire.Bind(new(service.NoteMapper), new(*mapper.NoteMapper)),
)

var ServiceProviders = wire.NewSet(
    impl.NewNoteService,
)

var ControllerProviders = wire.NewSet(
    controller.NewNoteController,
    provideSystemController,
    provideRealtimeController,
)
```

`NewNoteService` 已返回服务接口，所以这里无需额外绑定它。返回具体实现的构造函数，则用 `wire.Bind` 将实现绑定到使用方声明的接口。需要把配置转换成控制器参数时，参考 [infrastructure.go](../pkg/wire/infrastructure.go) 的 `provideSystemController`，在装配层完成适配。

[wire.go](../pkg/wire/wire.go) 的唯一 injector 按字段类型组装资源和 Container，新增 Controller 字段会成为生成图的依赖：

```go
func buildWithWire(ctx context.Context, cfg configs.Config, opts bootstrap.Options) (*Container, func(), error) {
    panic(wire.Build(
        AllProviders,
        wire.Struct(new(bootstrap.Resources), "*"),
        wire.FieldsOf(new(bootstrap.Options), "EnableExamples"),
        wire.Struct(new(Container), "*"),
    ))
}
```

这段声明放在已有带 `wireinject` 标签的 wire.go，保留其 imports。`wire.Struct(..., "*")` 自动填充可注入字段；`cleanup`、`closeOnce` 通过 `wire:"-"` 排除，交由公共生命周期方法管理。完成字段和 provider 修改后生成并检查：

```powershell
go generate ./pkg/wire
go tool wire diff ./pkg/wire
go test ./pkg/wire ./pkg/router/... -count=1
```

普通构建使用已提交的 wire_gen.go，无须运行生成器；不要手改该文件。Wire 声明使用 `wireinject` 标签，业务代码不导入 Wire DSL。

可选 `wire.NewContainerManual(ctx, cfg, opts)` 对应 CLI 的 `-di manual`。在 [manual.go](../pkg/wire/manual.go) 的 `buildManual` 中按实际顺序增加普通构造调用，并将结果写入 `&Container{...}`：

```go
noteMapper, err := mapper.NewNoteMapperWithReader(cluster)
if err != nil {
    return nil, nil, err
}
noteService := impl.NewNoteService(noteMapper)
noteController := controller.NewNoteController(noteService)
```

该摘录位于已有 buildManual 中，完整 Container 字面量与 cleanup 见源码。两条装配路径共用校验、Prepare 和关闭顺序；使用手写路径时同步构造和字段赋值，共享资源仍由容器统一释放。

CLI 在 Container 完成 Prepare 后执行下面的路由步骤，错误按现有启动路径返回并关闭资源：

```go
engine, err := router.NewEngine(container)
if err != nil {
    return err
}
if err := router.New(engine, container); err != nil {
    return err
}
```

`NewEngine` 配置 Gin 引擎和通用 HTTP 处理链，`New` 只组织路由组并调用模块登记函数。模块签名为 `RegisterSystemRoutes(container, public, protected)`、`RegisterNoteRoutes(container, protected)` 和 `RegisterRealtimeRoutes(container, public, protected)`；完整可复制的 Note 模块见 [分层示例](LAYER_EXAMPLES.md#routes用原生-gin-声明路径与权限)。中央入口保留现有 `/api/v1` 分组，新业务模块在正式调用区登记，教学 Note/realtime 保留 `EnableExamples` 条件。

wire 包不 import router。`bootstrap.Options` 只包含 EnableExamples、InitializeSchema、Settings；`cmd/example` 的开关用于教学，真实业务应在正式模型/路由列表中显式启用。

## 4. 事务如何传播

笔记 CAS 的条件更新是一条原子 SQL；MySQL 为保证返回本次更新行，在 Mapper 内使用事务完成 UPDATE 和回读。业务中“多次写入必须一起成功”时，仍由应用用例定义外层事务边界，并把同一个 tx 注入所有相关 Mapper。

下面是可放在当前 Go module 的应用用例包中的完整示例，不会自动注册到任何路由。两次创建任何一次失败都会回滚；构造、输入类型均使用当前真实 API。

```go
package usecase

import (
    "context"
    "errors"

    "healgo-scaffold/pkg/serve/mapper"
    "healgo-scaffold/pkg/serve/service"
    "healgo-scaffold/pkg/serve/service/impl"
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

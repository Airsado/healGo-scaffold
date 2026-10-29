# Container 与依赖注入

## 职责

应用组合根。参考 gin-scaffold，以 Google Wire v0.7.0 为默认装配方式，集中维护基础设施、Mapper、Service、Controller 的 provider sets。另保留普通构造函数注入示例，便于学习和需要时手写装配。Container 只在启动和路由层传递，controller/service 使用明确的构造参数。

## 内容

| 文件 | 负责什么 |
|---|---|
| [container.go](container.go) | Container、默认 NewContainer、统一 finish/Prepare 与幂等 Close |
| [manual.go](manual.go) | 可选 NewContainerManual 与 buildManual，直接调用 mapper → impl → controller |
| [infrastructure.go](infrastructure.go) | 配置校验、日志、DB/Auth/Settings/Streams 和系统控制器的 provider |
| [providers.go](providers.go) | 集中 InfrastructureProviders、MapperProviders、ServiceProviders、ControllerProviders、AllProviders |
| [wire.go](wire.go) | 唯一生成图 buildWithWire 声明 |
| [wire_gen.go](wire_gen.go) | 编译期生成的显式调用和逆序 cleanup，纳入版本管理 |
| [wire_test.go](wire_test.go) | 两种装配、初始化、失败清理与生命周期集成测试 |

```text
NewContainer → buildWithWire ────────┐
                                    ├→ Container → finish/Prepare
NewContainerManual → buildManual ───┘            → router.NewEngine → router.New → Runtime

Cluster → NoteMapper → service.NoteMapper → impl.NewNoteService → NoteController
Cluster.Primary → Auth → Streams
Cluster.Primary → Settings
```

两条公共构造函数签名相同：`(context.Context, configs.Config, bootstrap.Options) (*Container, error)`。CLI 默认 `-di wire`，`-di manual` 切换手写路径。容器保存 Config、共享 Resources、EnableExamples 和 mapper/service/controller；资源只构造一次。

MapperProviders 已有真实绑定，并非空占位：

```go
var MapperProviders = wire.NewSet(
    mapper.NewNoteMapperWithReader,
    wire.Bind(new(service.NoteMapper), new(*mapper.NoteMapper)),
)
```

此摘录来自 providers.go；完整 imports 与 wireinject 标签见源文件。NewNoteService 返回 service.NoteService，控制器只接收接口。

唯一注入器在 wire.go，通过字段装配生成普通 Go 构造调用：

```go
panic(wire.Build(
    AllProviders,
    wire.Struct(new(bootstrap.Resources), "*"),
    wire.FieldsOf(new(bootstrap.Options), "EnableExamples"),
    wire.Struct(new(Container), "*"),
))
```

`wire.Struct` 按字段类型组装共享资源和容器，新增 Controller 不需要修改长参数容器构造函数。`Config` 来自已完成校验的入口配置，`wire.FieldsOf` 从启动选项提供示例开关。`cleanup` 和 `closeOnce` 使用 `wire:"-"` 跳过生成装配，由 finish 和 Close 管理。

## 新增模块改哪里

1. 在 mapper/service/impl/controller 编写普通构造函数，参数声明需要的依赖，业务代码不用导入 Wire
2. 在 Container 增加路由需要访问的 Controller 字段；需要暴露其他启动依赖时同样添加明确类型字段
3. 在 providers.go 对应层加入构造函数；实现类型与参数接口不同时添加 `wire.Bind`
4. 运行 `go generate ./pkg/wire`，再用 `go tool wire diff ./pkg/wire` 检查生成结果
5. 在 `pkg/router/routes` 编写模块路由函数，并在路由总入口登记；路由只绑定已有 Controller

若同时维护手写装配，需同步 manual.go 的普通构造调用和最终 Container 字段赋值。新增模块不需要额外创建一套 Initialize 注入器。完整 Controller → Service → Mapper 示例见 [分层示例](../../docs/LAYER_EXAMPLES.md)。

## 约定

- 构造函数只绑定依赖，不查表、建表或注册路由。公共 finish 调用 Prepare，集中准备模型和种子；只有成功后 CLI 才构造 router，wire 不 import router。
- 打开资源的 provider 返回对象、cleanup、error。后续构造失败时逆序释放已成功创建的资源；Prepare 失败也关闭 Container。
- Container.Close 使用 sync.Once，清理 Streams → Auth → DB。Runtime 在单次 ShutdownTimeout 内尝试排空 HTTP 和 WebSocket；provider cleanup 关闭 manager 后清理依赖，不追加等待。
- 未使用流功能时 manager 不启动 goroutine。仅 EnableExamples 控制示例模型/路由，对象构造本身不执行示例 SQL。
- provider 声明有 wireinject 标签；修改图后生成并提交，不手改 wire_gen.go。普通构建不运行生成器。

在模板根目录运行：

```powershell
go generate ./pkg/wire
go tool wire diff ./pkg/wire
go test ./pkg/wire -count=1
```

只有这一张 Wire 图。集成测试使用 HEAGO_TEST_DATABASE_URL（PostgreSQL）或 HEAGO_TEST_MYSQL_DSN（MySQL），两个驱动分开运行并清理随机 schema/database。新增资源的完整接入步骤与事务传播见 [扩展手册](../../docs/EXTENDING.md)。

数据库资源由 `OpenCluster` 一次构造，普通服务只接收 Primary；需要允许延迟的 Mapper 显式接收 Cluster。笔记使用 `NewNoteMapperWithReader(cluster)`，只有 List 走显式 Read；认证/配置和详情等仍固定主库。Container 负责统一关闭主副本全部池，不能只关闭旧的单 DB。详见 [数据库手册](../../docs/DATABASE.md)。

# Container 与依赖注入

## 职责

应用组合根。默认使用可直接阅读和修改的普通构造函数注入；需要生成检查时切换 Google Wire v0.7.0。Container 只在启动和路由层传递，controller/service 不把它当全局服务定位器。

## 内容

| 文件 | 负责什么 |
|---|---|
| [container.go](container.go) | Container、NewContainerWithWire、统一 finish/Prepare 与幂等 Close |
| [manual.go](manual.go) | 默认 NewContainer 与 buildManual，直接调用 mapper → impl → controller |
| [infrastructure.go](infrastructure.go) | 配置校验、日志、DB/Auth/Settings/Streams、系统控制器和容器构造适配 |
| [providers.go](providers.go) | 集中 InfrastructureProviders、MapperProviders、ServiceProviders、ControllerProviders、AllProviders |
| [wire.go](wire.go) | 唯一生成图 buildWithWire 声明 |
| [wire_gen.go](wire_gen.go) | 编译期生成的显式调用和逆序 cleanup，纳入版本管理 |
| [wire_test.go](wire_test.go) | 两种装配、初始化、失败清理与生命周期集成测试 |

```text
NewContainer → buildManual ─┐
                           ├→ Container → finish/Prepare → cmd 调用 router.New
NewContainerWithWire       │                            → Runtime
  → buildWithWire ─────────┘

Cluster → NoteMapper → service.NoteMapper → impl.NewNoteService → NoteController
Cluster.Primary → Auth → Streams
Cluster.Primary → Settings
```

两条公共构造函数签名相同：`(context.Context, configs.Config, bootstrap.Options) (*Container, error)`。CLI 默认 `-di manual`，`-di wire` 切换生成路径。容器保存 Config、共享 Resources、EnableExamples 和 mapper/service/controller；资源只构造一次。

MapperProviders 已有真实绑定，并非空占位：

```go
var MapperProviders = wire.NewSet(mapper.NewNoteMapperWithReader,
    wire.Bind(new(service.NoteMapper), new(*mapper.NoteMapper)))
```

此摘录来自 providers.go；完整 imports 与 wireinject 标签见源文件。NewNoteService 返回 service.NoteService，控制器只接收接口。

## 约定

- 新增业务先改 manual.go 的显式构造、Container/容器构造参数，再同步集中 provider sets；业务文件不导入 Wire DSL。
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

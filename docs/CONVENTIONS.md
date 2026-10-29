# 目录与编码约定

## 1. 固定目录与依赖

本模板吸收 gin-scaffold 的固定 MVC 组织方式。只有通用根入口与独立示例两个程序：根 `main.go` 默认关闭示例，`cmd/example` 通过 `EnableExamples` 开启示例模型/路由。默认启动执行 `go run .`；源码存在不等于启用路由。

| 目录 | 职责 | 约束 |
|---|---|---|
| `main.go`、`cmd` | 薄入口与公共 Run、命令、信号 | 不写业务和 SQL |
| `configs` | YAML、校验、私有配置 | 连接和加密主密钥保留在文件 |
| `internal/model/<资源>` | 表结构、约束、稳定错误 | 不查询、不迁移；在 get_all_models.go 集中登记 |
| `internal/auth/settings/stream` 等 | 认证、配置与传输基础能力 | 不反向依赖 controller 或具体业务实现 |
| `pkg/router/router.go` | 全局中间件、根路由、开关 | 接收已初始化 Container，不构造服务 |
| `pkg/router/routes` | 资源路径、方法、权限与 handler | 不写请求绑定、业务规则和 SQL |
| `pkg/serve/controller` | HTTP、身份、参数、状态与 VO | 依赖服务接口，不接收 DB |
| `pkg/serve/controller/dto` | 请求 JSON 字段与形状校验 | 不接受 owner 等可信字段 |
| `pkg/serve/service` | 用例接口、普通 Go 输入/结果、窄 mapper 接口 | 不导入 Gin/GORM/具体 mapper |
| `pkg/serve/service/impl` | 业务校验、规则、用例实现 | 依赖契约，不依赖 HTTP 或 SQL |
| `pkg/serve/mapper` | 参数化 SQL、owner、CAS、错误脱敏 | 注入 DB/tx，所有 I/O 接受 ctx |
| `pkg/vo` | 明确公开的响应字段与转换 | 不嵌入敏感模型 |
| `pkg/wire` | Container、手写装配、集中 provider sets、可选生成 | 不 import router，不作全局服务定位器 |
| `pkg/bootstrap` | 模型/种子顺序、Runtime、关闭 | 不重复组装业务对象 |

运行时调用方向为 routes → controller → service 接口 → impl → mapper → model/DB；service 使用自己声明的 NoteMapper，组合根注入具体实现。根 `main.go` 调用 `cmd.Run` 处理进程启动，`service` 是用例契约；数据访问统一命名 mapper，不再叠加同义 repository/dao。

## 2. 文件与命名

资源名体现在文件、类型和构造函数中，例如 `note_controller.go` / `NoteController` / `NewNoteController`，`note_service.go` / `NoteService`，`note_service_impl.go` / `NoteServiceImpl` / `NewNoteService`，`note_mapper.go` / `NoteMapper` / `NewNoteMapper`。多个资源共用同一层包，输入类型也使用 `CreateNoteInput` 等明确名称。

小功能每层可以只有一个文件；没有持久化的 realtime 示例不创建空 model/mapper/service。构造函数只保存依赖，不查表、AutoMigrate、发网络请求或启动无退出机制的 goroutine。详细真实代码见 [分层示例](LAYER_EXAMPLES.md)。

## 3. HTTP、DTO、VO 与身份

- `/api/v1` 前缀由 router 总入口提供，资源 routes 声明相对路径，避免覆盖 `/auth`、`/system`。
- 登录校验与操作权限分别声明。owner 来自 `middleware.Principal(c)`，不能从请求体提交；有读权限也不能读取其他 owner 的资源。
- DTO 检查 HTTP 格式、类型、必填和未知字段；impl 独立校验业务不变量，保护任务/命令行等其他入口。
- 表模型不直接序列化。VO 只列公开字段，ID 使用字符串，revision 为数字，分页空集合为 `[]`。
- 使用统一 BindJSON/Write/Success/Fail。错误保留正确状态，不使用 HTTP 200 加错误字符串。
- WS/SSE 使用显式封装和独立最长时长，普通 API 仍有普通超时。回调只持有独立 ctx 与协议对象，不保存 Gin Context；首帧或升级后不能追加 JSON envelope。
- WS 必须持续读取以驱动 ping/pong/close。浏览器通过短期单次 ticket 握手，不把普通 bearer 放 URL；Origin 白名单不代替认证。

## 4. 数据、并发和事务

所有 mapper I/O 使用 `WithContext(ctx)`，条件使用占位参数。owner 范围进入查询、统计、修改与删除 SQL；不能先按 ID 取出数据再依赖 controller 过滤。

唯一、非空、外键等约束由数据库最终兜底；service/impl 执行业务规则。先查询“是否存在”不能代替并发唯一约束。更新字段由代码列举，map 保留空字符串等零值意图。

修改/删除携带旧 revision，在同一 SQL 中比较并更新。检查 RowsAffected，未命中固定主库，按当前 owner 下的存在状态区分未找到和冲突。PostgreSQL 用 RETURNING 返回本次更新结果，MySQL 在同事务内带锁回读；不能写完再从副本读结果。

默认所有操作走主库，只有允许复制延迟的用例在 Mapper 内显式调用 Cluster.Read。认证、权限、配置、详情、事务、迁移及冲突判断固定主库；同一个事务内所有 Mapper 绑定同一 tx。副本回调只做安全可重试读取，具体规范见 [数据库手册](DATABASE.md)。

单条原子 SQL 不额外套事务。多次写入须一起提交时，由应用用例建立事务，并把同一个 tx 传给所有相关 mapper；impl 保持接口依赖。完整代码见 [事务传播](EXTENDING.md#4-事务如何传播)。AutoMigrate 不代替复杂升级的版本化 SQL。

## 5. 装配与生命周期

默认 `wire.NewContainer` 在 manual.go 手写构造；可选 `NewContainerWithWire` 使用唯一生成图。providers.go 按 Infrastructure/Mapper/Service/Controller/All 集中声明。新增依赖同步 Container、手写路径与 provider sets，执行生成和差异检查，不手改 wire_gen.go。

两种路径都完成 Prepare 后才创建路由；Prepare 集中检查/初始化模型，再补缺失账号与配置。构造失败逆序清理已创建资源。共享资源由 Container 持有，业务层不能关闭注入的 DB/Auth/Streams。

Runtime 统一取消流并尝试排空 HTTP 与 WebSocket，两者共用单次 ShutdownTimeout；Close 复用排空结果，不增加等待预算。最终按 Streams → Auth → DB 清理。回调和 I/O 必须响应取消，不能遗留脱离 ctx 的后台任务。

服务返回稳定 sentinel，上层使用 errors.Is，不解析错误文本。日志禁止正文、token、密码、原始 SQL 参数或 panic 值；驱动错误跨层前脱敏。

## 6. 注释、文档与验证

每个维护目录附 README，统一“职责 / 内容 / 约定”；父目录导航到子目录。学习代码有效注释目标至少 20%，解释原因、不变量和失败处理；生成文件不为比例手改。`.idea/.local/.build`、缓存与数据库卷不纳入目录文档，也不提交私有配置。

改构造图检查 `go tool wire diff ./pkg/wire`，改 Go 运行适当测试和 vet，改文档检查源码摘录与链接。数据库依赖的行为使用真实 PostgreSQL 和 MySQL 分别运行，skip 不能计为通过。

新增资源至少验证主流程、401/403、owner 隔离、非法输入不落库、唯一冲突、旧版本冲突、并发不静默覆盖。流功能另测首帧、读写与慢客户端边界、来源、ticket、撤销和停机。事件 ID 不代表已有历史/断线重放。

核心场景能演示、数据与权限边界通过、文档可复现即可结束本轮，后续由真实需求驱动。

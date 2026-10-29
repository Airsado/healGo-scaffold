# heaGo-scaffold

一个可独立复制使用的 Go 后端模板：Gin + PostgreSQL/MySQL 8 + GORM + Sa-Token-Go。采用固定 MVC 分层目录；启动配置放文件，可编辑配置持久化到数据库。默认单库，支持可选单主单副本读写分离。

参考 [Done-0/gin-scaffold](https://github.com/Done-0/gin-scaffold/tree/22e3136bc4d09505824f2ca391d6fa20309790b6) 的 main/cmd 入口、集中模型登记、routes、controller、service/impl、mapper 和 VO 组织方式。默认使用手写构造函数注入，同时保留集中 provider sets 与可选 Wire 生成。

## 从哪里开始写业务

直接从 [Controller / Service / Mapper 规范与代码示例](docs/LAYER_EXAMPLES.md)开始。各层 README 展示真实构造函数、Go 代码、边界与错误处理；运行完整请求链再看 [开发手册](docs/DEVELOPMENT.md)。

| 层 | 写什么 | 规范与完整源码 |
|---|---|---|
| routes | URL、HTTP 方法、操作权限 | [routes](pkg/router/routes/README.md)、[note_routes.go](pkg/router/routes/note_routes.go) |
| controller | HTTP 参数、可信身份、调用用例、状态码 | [controller](pkg/serve/controller/README.md)、[note_controller.go](pkg/serve/controller/note_controller.go) |
| dto / vo | 请求字段 / 公开响应字段 | [DTO](pkg/serve/controller/dto/README.md)、[VO](pkg/vo/README.md) |
| service / impl | 用例契约 / 业务规则实现 | [service](pkg/serve/service/README.md)、[impl](pkg/serve/service/impl/README.md) |
| mapper | 参数化 GORM 查询、owner 与版本条件、错误分类 | [mapper](pkg/serve/mapper/README.md)、[note_mapper.go](pkg/serve/mapper/note_mapper.go) |
| model | 表结构、约束、集中模型登记 | [model](internal/model/README.md)、[note.go](internal/model/note/note.go) |

`service` 是业务用例契约，`service/impl` 实现规则；根 `main.go` 调用 `cmd.Run` 处理进程启动。数据访问统一命名 mapper，不再叠一层同义的 repository。

启动数据库并生成私有配置后，在本目录运行（默认 Compose 使用 PostgreSQL）：

```powershell
go run . migrate -config configs/config.local.yaml
go run . serve -config configs/config.local.yaml
```

根入口是通用服务，默认不登记示例模型或路由。需要运行完整教学用例时，改用独立示例程序：

```powershell
go run ./cmd/example migrate -config configs/config.local.yaml
go run ./cmd/example serve -config configs/config.local.yaml
```

示例地址为 **http://127.0.0.1:18082**。示例程序通过 `EnableExamples` 显式开启笔记 CRUD 和 realtime；迁移会创建 `example_notes`，realtime 不增加表。项目只有通用根入口和独立示例这两个程序，共用 `cmd/gin_server.go` 的命令处理。浏览器 SSE/WS、短期 ticket、心跳和代理见 [流式通信手册](docs/STREAMING.md)。新增业务按 [扩展手册](docs/EXTENDING.md)接入固定各层、Container、路由和集中模型列表。
## 已具备的能力

- 严格 YAML 配置、启动校验、结构化日志、requestId、统一 JSON 响应。
- PostgreSQL/MySQL 8 连接池、连接重试、GORM 模型集中注册、显式初始化命令。
- 可选读写分离：默认主库，显式允许延迟的 List 走副本，连接故障可受控回主库一次。
- Sa-Token-Go 随机 Bearer 会话；bcrypt 密码；账号、角色、权限持久化。
- 禁用/改密使旧会话失效；权限修改即时生效；保护最后一个有效管理员。
- 注册式数据库配置：类型校验、revision 并发保护、同事务审计；密钥类配置加密且 API 不回显。
- 请求体限制、登录限流、明确的 CORS/代理信任、panic 恢复、健康检查、优雅停机。
- WebSocket/SSE 显式封装：共享连接容量、独立最长时长、心跳、写入期限、周期鉴权和停机回收。
- 独立 PostgreSQL Docker Compose 部署；双驱动集成测试入口及 Linux race 检查。
- 每个项目目录提供 README，统一说明职责、内容和维护约定；Wire 生成代码纳入版本管理。

## 目录与依赖方向

```text
heaGo-scaffold/
├── main.go                     # 默认入口 → cmd.Run
├── cmd/
│   ├── gin_server.go            # 公共命令、配置、注入选择与生命周期
│   └── example/                 # 显式开启示例的薄入口（18082）
├── configs/                     # YAML、严格解析与启动校验
├── internal/
│   ├── auth/                    # 账号、角色、Sa-Token 适配
│   ├── db/                      # GORM、连接池、schema 初始化
│   ├── model/
│   │   ├── get_all_models.go    # 集中登记系统和可选示例模型
│   │   └── note/                # 笔记表模型与稳定错误
│   ├── logger/                  # slog
│   ├── middleware/              # HTTP、身份、权限
│   ├── response/                # 统一响应与严格 JSON 绑定
│   ├── settings/                # 数据库配置、加密、CAS、审计
│   └── stream/                  # WebSocket/SSE 与共享生命周期
├── pkg/
│   ├── bootstrap/              # 模型准备、种子、HTTP 与资源关闭
│   ├── router/
│   │   ├── router.go           # 路由总入口
│   │   └── routes/             # system/note/realtime 路由
│   ├── serve/
│   │   ├── controller/dto/     # HTTP 控制器与请求 DTO
│   │   ├── service/impl/       # 用例接口与实现
│   │   └── mapper/             # GORM 数据访问
│   ├── vo/                     # 公开响应与转换
│   └── wire/                   # Container、手写注入、集中 providers、可选 Wire
├── docs/
├── scripts/
├── Dockerfile
└── compose.yaml
```

CLI 默认调用 `wire.NewContainer(ctx, cfg, opts)` 手写注入，`-di wire` 切换为同签名的 `NewContainerWithWire`。Container 完成集中模型准备与种子后，调用 `router.New(container)`，再交给 Runtime 监听。wire 不反向 import router，避免循环依赖。

HTTP 控制器处理参数和状态码；service/impl 实现规则；mapper 执行 SQL；VO 选择公开字段。停机在同一预算内排空 HTTP 和 WebSocket，再按 Streams → Auth → DB 清理资源。`internal` 包仅供当前 Go module 使用；固定目录的命名与依赖见 [目录规范](docs/CONVENTIONS.md)，手写注入与 Wire 图见 [依赖注入说明](pkg/wire/README.md)。
## 文件配置与数据库配置

| 放在哪里 | 内容 | 修改生效方式 |
|---|---|---|
| 私有 YAML | 数据库连接、HTTP/日志、安全边界、会话期限、初始化管理员、加密主密钥 | 重启进程 |
| PostgreSQL / MySQL | 账号/密码哈希、角色权限、注册的系统和业务配置、配置审计 | 后续请求读取生效 |

数据库连接必须先存在，才能读取数据库中的配置，所以连接信息保留在文件。加密主密钥也保留在文件，避免数据库备份同时包含解密钥匙。当前默认注册 `system.display_name`；新增键先在代码里声明类型、默认值和校验器，然后才允许 API 修改。

默认值只补缺失项。修改 YAML 中的管理员密码、修改代码默认值或重启，均不会覆盖已有账号和数据库配置。`bootstrap.admin_password` 仅用于首次创建；后续改密调用 API。

PostgreSQL、MySQL 与主副本分别参考 [配置模板](configs/README.md)。`database.driver` 省略时仍为 `postgres`，设为 `mysql` 使用 MySQL 8；启动不会自动搬迁已有数据。主副本地址和凭据都保留文件配置，副本使用单独只读账号。认证、权限、配置、详情、写事务及冲突判断始终走主库；仅明确接受复制延迟的查询调用 `Cluster.Read`，笔记 List 提供真实示例。分流、事务、回退及运维边界见 [数据库手册](docs/DATABASE.md)。

secret 配置使用 AES-256-GCM，读取 API 只提供 `secret/configured/revision` 等元数据。业务通过 `Settings.Value(ctx, key)` 在进程内部读取 JSON 明文，不能把结果直接返回前端。`value: null` 显式清空 secret；省略 `value` 是参数错误。

## Docker 启动

需要 Docker Compose v2。配置生成脚本在 Windows 上需要 PowerShell 7，在 Linux/macOS 上需要 OpenSSL。以下命令均在 **本目录** 运行，部署项目名固定为 `heago-scaffold`。

```powershell
# PowerShell 7：生成随机密码和密钥，已存在时不会覆盖
pwsh -NoProfile -ExecutionPolicy Bypass -File scripts/init-config.ps1
docker compose --env-file .local/compose.env up -d --build
docker compose --env-file .local/compose.env ps -a
```

```sh
# Linux/macOS
sh scripts/init-config.sh
docker compose --env-file .local/compose.env up -d --build
```

Dockerfile 支持通过构建参数指定 Go 模块源。使用国内模块源时，可先执行 `docker compose --env-file .local/compose.env build --build-arg GOPROXY=https://goproxy.cn,direct`，再执行 `docker compose --env-file .local/compose.env up -d`，不修改宿主机 Go 设置。

初始化脚本生成下列私有文件，均被 Git 忽略：

- `configs/config.local.yaml`：本机开发配置（HTTP 18081，数据库 55434）。
- `configs/config.docker.yaml`：容器配置（HTTP 8080，数据库 postgres:5432）。
- `.local/postgres_password`：PostgreSQL 初始化密码文件。
- `.local/compose.env`：配置文件的宿主机组 ID，不存业务配置。

Linux 上脚本限制文件权限，并让非 root 容器通过同组读取配置。不要把这几份文件提交 Git 或发到日志。默认账号为 `admin`，随机密码在私有 YAML 的 `bootstrap.admin_password` 字段中查看；没有通用默认密码。

服务地址为 **http://127.0.0.1:18081**，这是 API 服务。探活为 `/healthz`，数据库就绪检查为 `/readyz`。`migrate` 容器成功退出 0 是正常状态，随后 app 启动。

Compose 使用独立的 `pgdata` 卷保存数据。普通停止使用 `docker compose --env-file .local/compose.env down`；不要给保留数据的环境加 `-v`。默认仅在 loopback 暴露 HTTP；外部访问通过你的 HTTPS 反向代理配置。

## 本机开发

Go 最低版本见 `go.mod`，当前最低与构建版本均为 Go 1.26.10。

```powershell
# 只启动此模板的数据库，额外开放本机端口 55434
docker compose --env-file .local/compose.env -f compose.yaml -f compose.dev.yaml up -d postgres
go run . migrate -config configs/config.local.yaml
go run . serve -config configs/config.local.yaml
```

不要同时启动容器 app 和本机 server，它们的宿主机 HTTP 端口相同。`config.example.yaml` 包含不可运行的占位符，正式配置需要生成或填写真实值。

## 初始化与升级

`migrate` 使用 GORM AutoMigrate 创建/补充全部注册模型，同时补齐初始管理员和缺失的配置项。普通启动默认 `database.auto_migrate: false`，先检查表是否存在，再启动服务；本地开发可以显式打开该开关。

AutoMigrate 适合首次建表及简单增量。它不是完整的版本化迁移工具：表存在检查也不代表列/约束已与新版本一致。字段重命名、数据回填、删除和复杂索引变更应另写经审查的 SQL，并在备份与演练后随版本执行。初始化只在主库执行，通过 PostgreSQL advisory lock 或 MySQL 同连接 GET_LOCK 防止并行迁移；MySQL DDL 可能隐式提交，不能承诺整个初始化失败后自动回滚。副本结构由部署侧的数据库复制同步。

升级镜像时，先备份，再显式运行新版本 `migrate`，再更新 app：

```sh
docker compose --env-file .local/compose.env build
docker compose --env-file .local/compose.env run --rm migrate
docker compose --env-file .local/compose.env up -d app
```

数据库可用 PostgreSQL 的 `pg_dump -Fc`、MySQL 的逻辑或物理备份方案；恢复先演练到独立数据库。私有 YAML 中的 `security.encryption_key` 要单独备份并保留原值；丢失或更换它会使已有 secret 无法解密，当前没有自动轮换流程。

## 开发和验证入口

```sh
go generate ./pkg/wire
go tool wire diff ./pkg/wire
go test ./...
go vet ./...
go build -o .build/heago-server .
go build -o .build/heago-example ./cmd/example
```

真实数据库测试使用 `HEAGO_TEST_DATABASE_URL`（PostgreSQL URL）或 `HEAGO_TEST_MYSQL_DSN`（MySQL DSN），两者均未设置时明确 skip。分别运行两种驱动；同时设置只会优先 PostgreSQL。测试创建并清理随机 schema/database，只能指向专用测试实例。Linux 有 C 编译器时运行 `go test -race ./...`。测试环境变量只是测试入口，应用启动仍读取 YAML；实际覆盖范围见验证记录。

Wire 工具版本固定在 `go.mod`，也可直接运行 `go tool wire gen ./pkg/wire`。所有 providers 集中在一张图。构造关系变化后同步手写注入，重新生成并提交 `wire_gen.go`；普通构建无需运行生成器。CI 检查这一张依赖图以及两种装配路径的测试。

详见 [开发手册与完整示例](docs/DEVELOPMENT.md)、[数据库与读写分离](docs/DATABASE.md)、[框架 API](docs/API.md)、[模块扩展](docs/EXTENDING.md)、[目录规范](docs/CONVENTIONS.md)、[流式通信](docs/STREAMING.md)、[验证记录](docs/VERIFICATION.md)。

## 当前边界

会话使用 Sa-Token-Go 内存存储：重启需要重新登录，只部署一个 app 实例。账号、权限、设置及审计保存在数据库，重启会保留。数据库读副本不会把会话变成分布式状态；多个 app 实例应先实现共享会话存储并完成相应并发测试，当前模板未引入 Redis。

登录限流是单进程全局每分钟预算，用于限制 bcrypt 开销；需要公网多实例限流时，在网关接入更细粒度的策略。请求超时通过 context 传播，业务 I/O 必须接受该 context，不能强制打断任意不合作的函数。

WebSocket/SSE 默认共享每进程 128 条活动流容量，单连接最长五分钟；显式流封装使用自己的期限，普通 API 超时保持原约定。当前不保存事件历史、不自动重放断线消息；应用回调必须响应取消，流开始后不能再追加 JSON 错误响应。

本轮交付的停止点是：独立启动、认证权限可验证、配置持久化不丢、业务接入边界明确。后续功能以实际项目需求为准。

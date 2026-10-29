# 验证记录

最近验证：2026-10-29。当前工具链为 Go 1.26.10。以下按日期区分路由与 Wire 对齐、名称更正、安全修复、双数据库迭代及旧部署实测，不代表未来业务自动满足相同条件。

## 路由与 Wire 对齐 gin-scaffold（2026-10-29）

本轮对照 gin-scaffold 固定提交 `22e3136bc4d09505824f2ca391d6fa20309790b6` 的路由与 Wire 源码。默认 `NewContainer` 使用 Wire 生成图，可选手写入口为 `NewContainerManual`；CLI 默认 `-di wire`，`-di manual` 显式选择手写装配。按层 provider sets 保留，容器改用 `wire.Struct` 按字段装配，生命周期字段以 `wire:"-"` 排除，不再维护长参数容器构造函数。

`router.NewEngine(container)` 准备 Gin、全局中间件、探活与统一错误响应，`router.New(engine, container)` 只检查路由依赖、建立分组并显式挂载模块。模块统一以 Container 为首参，使用原生 Gin API；Container 不传入 Controller/Service。HTTP 路径、权限、示例开关及初始化/停机顺序保持原语义。开发与扩展文档已同步实际路由、字段与 provider 示例。

| 本轮实际检查 | 结果 |
|---|---|
| Windows / Go 1.26.10，全量 `go test -mod=readonly -count=1 ./... -timeout 180s` | 通过；此命令清空数据库测试变量，真实数据库测试在下列独立步骤执行 |
| 一次性 PostgreSQL 16 / MySQL 8.4，分别运行 `go test -mod=readonly -count=1 ./pkg/wire ./pkg/router/... -timeout 180s` | 两种驱动均通过；覆盖双装配初始化、示例隔离、系统/CRUD/流协议 HTTP 行为与清理 |
| 新增无数据库启动回归 | 引擎/路由分步登记、缺失依赖、无效代理脱敏、重复登记错误、登录限流预算均通过 |
| `go generate ./pkg/wire`、`go tool wire diff ./pkg/wire` | 真实生成成功，最终无生成差异 |
| `go vet ./...`、两个入口分别 `go build -mod=readonly`、`go mod tidy -diff` | 通过，无依赖差异 |
| 文档与格式 | 318 个相对文件链接、97 个开发文档文件链接/标题锚点检查通过；维护目录 README、注释和 `git diff --check` 通过 |

测试使用随机凭据、loopback 随机端口和隔离 schema/database，专用容器及其匿名数据卷已清理。未访问既有私有配置或修改运行中的应用；本轮未重跑 Linux race、主从故障、漏洞扫描或 Docker 镜像构建，下列相关结果仍属于此前对应迭代。

## 项目名称更正（2026-10-29）

项目显示名由 `heaGo-scaffold` 更正为 `healGo-scaffold`，Go 模块和所有内部导入路径改为 `healgo-scaffold`，构建产物改为 `healgo-server` / `healgo-example`，镜像使用 `healgo-scaffold:local`。README、NOTICE、配置示例、CI、MVC 文档及 Wire 生成代码同步新名称。

配置密文 v1 的 AAD 历史字节固定为版本常量，并用历史密文向量验证继续可解密。Compose 部署资源标识继续使用 `heago-scaffold` 以定位原有数据卷和网络；数据库、锁命名空间和 `HEAGO_` 环境契约沿用现有标识。未修改私有配置或数据库内容，也未移动本机检出目录。下方历史验收中的资源名、镜像标签和构建路径按当时实际值保留，不能当作改名后重新执行的证据。

本轮 Windows / Go 1.26.10 的全量 `go test -count=1 ./... -timeout 180s`、`go vet ./...`、Wire diff、两个新名称入口产物构建、`go mod tidy -diff`、Compose 配置校验及 297 个本地文档链接检查均通过。新增固定历史密文向量验证可读性及配置项绑定。未设置真实数据库测试变量，对应集成测试跳过；未重新执行主从、race、漏洞扫描或 Docker 镜像构建，下方结果对应此前安全补丁修复。

## 安全补丁与仓库审查修复（2026-10-29）

本轮仅修复依赖漏洞、无法兑现的权限通配模式，以及复制验收忽略数据库筛选三个问题，未增加业务或多数据源能力。

`go.mod` 最低版本、Docker builder 与 CI 统一为 Go 1.26.10，Docker 多架构摘要已与官方 Registry 核对。更新 `quic-go v0.59.1`、`x/net v0.62.0` 及其最低传递要求：`x/crypto v0.58.0`、`x/text v0.43.0`、`x/sys v0.49.0`、`x/sync v0.24.0`、`x/mod v0.41.0`、`x/tools v0.51.0`；Gin、GORM 和 Sa-Token 版本保持原值。

权限保存会在 SQL 前拒绝 `system:*:*`、`*:settings:*`、`*:*` 等 Sa-Token v0.2.3 无法按预期匹配的组合，继续支持 `*`、固定前缀以及末段为普通文本的分段通配。回归直接调用真实 Sa-Token 匹配器，覆盖允许模式的正反匹配及无效模式不访问数据库；规则详见[认证模块](../internal/auth/README.md)。

复制验收按 `All`、`Postgres`、`MySQL` 生成父测试和子测试两级锚定的 `-run` 正则，参数大小写会规范化。离线回归覆盖 30 组数据库筛选、复制开关、`-Race`、失败后停止、不误报成功、进程环境与工作目录恢复，并验证失败子进程返回非零退出码。另将原本不存在的环境变量精确恢复为不存在，兼容 PowerShell 对 `$null` 字符串参数的转换；回归仅使用临时副本、假连接和 mock Go。

| 本轮实际检查 | 结果 |
|---|---|
| Windows / Go 1.26.10，`go test -count=1 ./... -timeout 180s` | 通过；此轮未设置数据库测试变量，真实数据库与主从测试跳过 |
| 独立 PostgreSQL 16 / MySQL 8.4，分别运行 Windows 全量 `go test -count=1 -p 1 ./...` | 两种驱动均通过，使用一次性专用容器和隔离 schema/database |
| Linux / Go 1.26.10 + GCC 14.2.0，双驱动分别运行全量 `go test -race -p 1 ./... -count=1` | PostgreSQL 16.14 / MySQL 8.4.13 均通过，无 `DATA RACE`；专用测试容器与网络已清理 |
| `go vet ./...`、`go tool wire diff ./pkg/wire` | 通过 |
| 通用根入口、`cmd/example` 分别构建 | 通过，产物工具链为 Go 1.26.10 |
| `go mod tidy -diff`、`go mod verify` | 通过，无依赖差异，模块校验通过 |
| `pwsh -NoProfile -File scripts/replication/test-run-tests.ps1` | 30 组用例及失败子进程非零退出检查通过 |
| `go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...` | 退出码 0；符号可达漏洞 0，导入包漏洞 0，模块级记录 1 |
| Docker 镜像构建（本地导入的官方 builder） | 成功生成 `heago-scaffold:security-fixes`，构建路径与限制见下文 |

本机 Docker daemon 直连官方 Registry 时 DNS/网络超时，原始 `docker build` 在基础镜像元数据解析阶段失败。随后仅在本次进程设置代理，通过固定的官方多架构摘要下载对应 `linux/amd64` builder 并导入本地；临时 Dockerfile 只将首行 builder 改为该本地标签，其余构建内容与维护版一致，完整编译及运行层构建成功。维护版仍固定官方标签和多架构摘要，未修改 Docker 全局配置；本次替代路径不能视为本机原始在线拉取已恢复。

剩余模块级记录为 `GO-2026-5932`，对应已弃用且没有修复版本的 `golang.org/x/crypto/openpgp`；当前依赖图没有导入该包，项目密码散列仍使用同模块中的 bcrypt。该记录不能描述为可达漏洞，也不能声称全部模块级记录归零。源码扫描结果仅对应当前版本、平台和漏洞库状态。

CI 已加入固定版本漏洞扫描和复制脚本离线回归，保留原双数据库 race 矩阵；尚未实际触发托管 GitHub Actions。本轮没有运行会暂停复制或停止副本的主从故障验收，也没有替换现有运行中的应用。

## 本轮入口统一（2026-10-27）

通用服务统一由根 `main.go` 启动，独立教学示例保留在 `cmd/example`，项目只有这两个程序。两者共用 `cmd/gin_server.go` 的命令处理。默认开发命令为 `go run . migrate -config configs/config.local.yaml` 和 `go run . serve -config configs/config.local.yaml`；构建目标分别为：

```powershell
go build -o .build/heago-server .
go build -o .build/heago-example ./cmd/example
```

Dockerfile 和 CI 的通用服务构建目标统一为根目录 `.`。手写与 Wire 仍是两个程序均可选择的装配方式，`-di manual|wire` 及其测试保持原约定。README、开发手册、目录规范与工作流说明使用当前入口；下方历史构建结果仅表示当时全部程序已通过构建。

本轮实际检查：`go test ./... -count=1`、通用/示例入口构建、Wire diff 和 `go vet ./...` 均通过；未设置数据库测试变量，真实数据库集成用例明确跳过。Docker 镜像已按根入口重新构建成功。295 个本地文档链接有效，旧服务入口路径已从当前维护文件移除。

## 双数据库与显式读写分离

本轮支持两种 driver，保留原 PostgreSQL 单库 YAML。新配置支持一个主库和最多一个副本；普通 `*gorm.DB` 固定主库，只有显式 `Cluster.Read` 查询使用副本。两个连接池统一创建/关闭，MVC、手写注入与可选 Wire 的结构保留。

| 实际检查 | 结果 |
|---|---|
| 真实 PostgreSQL 16 / MySQL 8.4，分别运行全量 `go test -count=1 ./...` | 两种驱动通过；集成测试使用随机隔离 schema/database |
| Linux 容器、两种真实数据库，全量 `go test -mod=readonly -race -count=1 ./...` | 通过，无 race 报告 |
| 最后修正后的 db/mapper、就绪接口回归 | 双驱动通过；db/mapper 双驱动 Linux race、就绪接口 PostgreSQL Linux race 通过 |
| `TestReplicationIntegration` | 真实 PostgreSQL / MySQL 主从通过；不是两个独立数据库模拟复制 |
| `pwsh -NoProfile -File scripts/replication/run-tests.ps1 -IncludeReplication` | 最终完整串行入口通过：两种全量套件及两种真实主从故障测试；退出码 0 |
| `go tool wire diff ./pkg/wire`、`go vet ./...`、`go mod tidy -diff` | 通过；无生成图或依赖差异 |
| 当时全部程序分别构建 | 通过 |
| `docker build -t heago-scaffold:local .` | 成功构建；本轮没有替换已有运行中的应用 |

真实复制测试通过 Docker 主从暂停回放，确认：列表暂时看不到新行且不回退；详情仍立即读到主库；旧 revision 的真实 Controller 返回 HTTP 409；改密后旧 token 立即失效；配置更新及其 revision 立即可读。副本账号连接主库也无法写表，证明查询权限独立于副本只读模式。

停止副本后，允许回退的列表只回主库一次且返回完整分页；关闭回退时只尝试副本。空结果、not found、语法/权限错误、context 取消/超时不会因此回主库。恢复后使用禁止回退的连接确认副本追平，防止测试被主库结果误导；关闭集群后主副池均不可再 Ping。

事务回归覆盖：两个 Mapper 共享同一 tx 的未提交可见性及完整回滚、并发 CAS 只有一个成功、MySQL REPEATABLE READ 下更新和删除的 current read 分类。跨连接池的身份锁覆盖互斥、等待取消、持锁取消回滚及再次获锁；并发建管理员/配置补缺、大小写身份隔离、重复迁移、64 KiB 加密载荷与审计回滚也分别在两种数据库通过。主库连接不可用时 `/readyz` 返回 503，`/healthz` 继续返回 200；单独副本故障不使主库就绪失败。

一次完整套件后的 MySQL 复制基线等待曾在 15 秒内未追平；事后 IO/applier、GTID、权限与延迟检查正常，单独复制测试及“MySQL 全量套件后接双数据库复制”复测通过，尚未复现。保留原等待期限并补充阶段、错误类别、观察到的行数及 context 状态诊断；未通过自动回退或扩大等待隐藏失败。故障注入测试须串行执行，不能同时由其他测试暂停或停止相同节点。

配置示例与开发说明见 [数据库手册](DATABASE.md)、[配置目录](../configs/README.md)。复现工具见 [主从测试说明](../scripts/replication/README.md)：`replication.ps1 -Action Start` 后运行 `run-tests.ps1 -IncludeReplication`。测试连接与随机密码位于被忽略的 `.local/replication-test/`，不能作为部署凭据。

CI 已配置 PostgreSQL 16/MySQL 8.4 的单库矩阵，保留 Wire、vet、race，当前构建通用根入口与独立示例两个程序；双数据库迭代时工作流静态校验通过，尚未触发托管 GitHub Actions。该矩阵不包含需主动停副本的主从故障验收。该轮文档检查中维护目录 README 齐全，300 个本地文档链接有效，生产代码有效注释占非空行约 20.8%。

## 此前固定 MVC 目录与装配验证（2026-10-26）

| 实际检查 | 结果 |
|---|---|
| 真实 PostgreSQL：`go test -count=1 ./... -timeout 180s` | 通过 |
| Linux 容器、真实 PostgreSQL：`CGO_ENABLED=1 go test -mod=readonly -race -count=1 ./... -timeout 180s` | 通过，无 race 报告 |
| `go vet ./...` | 通过 |
| `go mod tidy -diff` | 通过，无差异 |
| `go tool wire diff ./pkg/wire` | 通过，无差异，当前仅一张图 |
| 当时全部程序分别构建 | 通过 |
| `docker build -t heago-scaffold:local .` | 成功构建镜像 |
| `TestExampleRoutesRequireExplicitEnablement` | 通过，覆盖 8 个示例路由的实际开关行为 |

当前布局采用根 main → 公共 cmd.Run，固定 model、routes、controller/dto、service/impl、mapper、VO 和 Container。默认 NewContainer 手写构造，可选 NewContainerWithWire 复用相同公共收尾。所有 providers 集中维护；集中模型准备完成后才创建路由。

文档同步后检查：33 个维护目录均有 README，子目录统一“职责 / 内容 / 约定”；265 个相对 Markdown 文件链接有效，旧布局路径与注册教程无残留。16 段 PowerShell 代码块语法解析通过；共享 tx 的完整 CreatePair 示例单独提取编译通过，临时文件已清理。Wire diff 与 diff 格式检查再次通过；这些文档检查未启动数据库或应用。

默认入口不登记 example_notes 或示例路由；cmd/example 通过 EnableExamples 开启。构造示例对象本身不执行 SQL，也不等于启用示例。测试检查两种装配、初始化与资源释放；命令统一支持 serve/migrate/healthcheck 和 -di manual|wire。

当时 CI 已同步为单张 Wire 图、vet、真实 PostgreSQL race 与当时全部程序的构建。工作流须将本模板作为独立仓库根目录后生效；尚未实际触发托管 GitHub Actions。该轮构建镜像不等同于重新部署应用。

## 认证、配置与 HTTP 边界

已有真实数据库与 HTTP 测试覆盖严格 YAML/JSON、未知字段、错误脱敏、连接重试与取消、初始化幂等、账号/角色/会话、最后一个管理员保护、secret 加密与篡改拒绝、配置 revision 并发写和同事务审计、CORS、413、panic 恢复和日志边界。本轮全量测试继续覆盖这些现有用例。

Sa-Token-Go v0.2.3 对默认 device 的多会话枚举需要适配，当前独立登录使用内部随机 device；上限行为有测试。库时间精度为秒，同秒登录不承诺严格按先后淘汰。UTF-8 密码按字节校验，初始化不覆盖改过的密码或已保存配置。

## 分层 CRUD 验证

- HTTP 覆盖登录/操作权限、严格输入、DTO/VO 字段、分页、owner 读写/删除隔离、重复 slug、主动清空正文及旧 revision 冲突。
- 真实 PostgreSQL 8 个并发编辑使用同 revision 时只有一个成功，其余返回冲突；已取消 ctx 下的查询/写入不产生额外数据。
- impl 单元测试通过 mapper 替身验证非法输入不执行持久化；mapper 无数据库测试检查构造不查询和驱动错误不泄漏。
- 此前独立二进制实测登录、CRUD、409、清空正文、停止重启后数据保留和旧会话失效；一次性数据库/配置在验收后清理。本轮新增结构检查不代替真实业务自身的事务测试。
- [分层规范](LAYER_EXAMPLES.md)、各层 README、[开发手册](DEVELOPMENT.md)和 [扩展手册](EXTENDING.md)已按固定目录重写，提供实际构造、规则、SQL、错误分类及共享 tx 的完整用例。

## WebSocket / SSE 验证

通用协议位于 internal/stream，WebSocket 固定依赖 github.com/coder/websocket v1.8.15。共享活动容量为每进程 WS/SSE 合计 128，默认最长五分钟。realtime 控制器/路由提供 SSE tick/done、WS 文本/二进制 echo 和 30 秒单次浏览器 ticket，不登记新业务表。

真实 HTTP/TCP 测试覆盖首帧 flush、心跳、多行/JSON、输入输出大小、慢客户端写入期限、取消阻塞写、等待发送者、ping/pong、Origin、周期授权和安全错误分类。慢写测试先观察 TCP 背压，避免依赖单帧大小或固定耗时。

Runtime 测试验证普通请求仍超时、显式流可跨普通预算、客户端断开保留真实取消、停机取消 WS/SSE 并等待回调。HTTP 排空与 WebSocket 等待共用单次 ShutdownTimeout，耗尽后 Close 不再次等待。

真实数据库/HTTP 覆盖 401/403、同源浏览器 WS、Bearer、ticket 单次/到期/容量/并发、原会话注销后握手拒绝，以及访问日志不包含 ticket/token。本轮 Linux 全量 race 包含上述协议、Runtime、controller/routes、Container 用例。

## 此前 Docker 部署实测

- Compose 项目 heago-scaffold，API 127.0.0.1:18081，开发 PostgreSQL 127.0.0.1:55434；曾实际验证 PostgreSQL healthy、migrate 退出 0、app healthy。
- app UID/GID 为 10001，根文件系统只读，配置只读挂载；应用目录写入被拒绝。
- HTTP 实测探活、管理员登录、错误密码/无会话 401、配置更新、旧 revision 409、审计、退出后 401；重启后旧 token 失效，配置仍在。
- 扫描 app/migrate 日志未发现生成的数据库密码、管理员密码、加密主密钥。pg_dump 备份恢复到一次性数据库，确认账号/配置/审计保留后清理。
- PowerShell/sh 私有配置生成与重复执行曾实测，第二次不覆盖密码/密钥；本轮未因目录调整更改初始化脚本。

## 使用边界

会话和登录限流为进程内状态，仅支持已验证的单实例使用，重启需重新登录。共享会话、多副本、压力容量和复杂版本化迁移未纳入本轮；GORM 表存在检查不证明列级兼容。WebSocket/SSE 不保存事件历史、不自动断线重放。新增业务需补自己的归属、约束、回滚与并发验证。

# 从一个请求开始开发

这份手册用已实现的笔记 CRUD 串起固定 MVC 目录：公共 cmd.Run → Container 装配 → 集中模型准备 → router/routes → controller → service/impl → mapper → 数据库，再转 VO 返回。先看 [真实分层代码](LAYER_EXAMPLES.md)，目录与边界见 [规范](CONVENTIONS.md)，新增资源见 [扩展手册](EXTENDING.md)。

## 1. 先把示例运行起来

以下命令在 **heaGo-scaffold 根目录**执行，使用 PowerShell 7.1+、Go 和 Docker。示例采用默认 PostgreSQL 部署，在本机运行并监听 `127.0.0.1:18082`。MySQL 8 或单主单副本的配置方式见 [数据库手册](DATABASE.md)，后面的 HTTP 脚本保持相同。

```powershell
pwsh -File scripts/init-config.ps1
docker compose -f compose.yaml -f compose.dev.yaml --env-file .local/compose.env up -d postgres
# 等待 PostgreSQL healthy 后，同一入口先迁移再启动
go run ./cmd/example migrate -config configs/config.local.yaml
go run ./cmd/example serve -config configs/config.local.yaml -di manual
```

serve 占用当前终端，另开终端执行后面的 HTTP 步骤；Ctrl+C 通过公共 Runtime 排空再关闭。

| 入口 | 示例开关 | 用途 |
|---|---|---|
| [根 main.go](../main.go) | false | 通用服务：`go run .`，不登记示例表或路由 |
| [cmd/example/main.go](../cmd/example/main.go) | true | 笔记 CRUD 和 realtime；默认端口 18082 |

这两个程序都调用 [cmd/gin_server.go](../cmd/gin_server.go) 的 Run，支持 serve、migrate、healthcheck、`-config`、`-addr` 和 `-di manual|wire`。默认通用服务使用 `go run . migrate`、`go run . serve`；本手册下面为了演示完整业务而运行独立示例。默认手写注入；需要对照生成结果时停止当前进程，再用 `-di wire` 启动相同程序。

启动连接和加密主密钥来自私有 YAML，可编辑配置、账号和笔记保存在所选数据库。初始管理员密码来自 bootstrap.admin_password；已有账号改密后使用当前密码，migrate 不重置它。完整配置结构见 [配置模板](../configs/README.md)。默认 Docker app 使用关闭示例的入口。
## 2. 先认识 HTTP 契约

所有笔记操作要求登录；权限与归属分别检查：拥有读权限也只能读取**自己创建的笔记**。初始管理员的 `*` 权限覆盖示例读写，但不会绕过 owner 条件读取他人的笔记。

| 方法 | 路径 | 权限 | 结果 |
|---|---|---|---|
| POST | `/api/v1/examples/notes` | `example:notes:write` | 201，新笔记 |
| GET | `/api/v1/examples/notes?page=1&pageSize=20` | `example:notes:read` | 200，分页列表 |
| GET | `/api/v1/examples/notes/:id` | `example:notes:read` | 200，单条笔记 |
| PUT | `/api/v1/examples/notes/:id` | `example:notes:write` | 200，新 revision |
| DELETE | `/api/v1/examples/notes/:id?revision=1` | `example:notes:write` | 200，`{"deleted":true}` |

创建请求：

```json
{"slug":"first-note","title":"第一条笔记","content":"正文允许为空"}
```

PUT 完整替换可变字段，title、content、revision 都必须提供，不能用 null 代替；slug 不可修改：

```json
{"title":"更新后的标题","content":"","revision":1}
```

响应沿用框架 envelope，示意如下；时间字段由服务端实际生成：

```json
{
  "data": {
    "id": "123",
    "slug": "first-note",
    "title": "第一条笔记",
    "content": "正文允许为空",
    "revision": 1,
    "created_at": "2026-10-26T04:00:00Z",
    "updated_at": "2026-10-26T04:00:00Z"
  },
  "requestId": "server-generated-uuid",
  "timeStamp": 1792987200000
}
```

ID 是 JSON 字符串，避免浏览器的大整数精度损失；revision 是数字。列表的 `data` 为 `{items: [], page: 1, pageSize: 20, total: 0}`，按 ID 倒序。page 为 1–10000，pageSize 为 1–100；缺省使用 1、20，未知、重复、空或带前导零的分页参数会被拒绝。

## 3. 用 PowerShell 跑通登录、CRUD 和冲突

以下代码只在内存保存 token，不输出密码或 token。先确认示例已启动；在提示中输入管理员的**当前密码**。`Read-Host -MaskInput` 隐藏键入内容，输入不要写成带密码的历史命令。

```powershell
$ErrorActionPreference = 'Stop'
$base = 'http://127.0.0.1:18082'
$headers = $null
$username = Read-Host '用户名（直接回车使用 admin）'
if ([string]::IsNullOrWhiteSpace($username)) { $username = 'admin' }
$password = Read-Host '当前登录密码' -MaskInput
$loginBody = @{ username = $username; password = $password } | ConvertTo-Json -Compress

try {
    $login = Invoke-RestMethod -Method Post -Uri "$base/api/v1/auth/login" `
        -ContentType 'application/json; charset=utf-8' -Body $loginBody
    $headers = @{ Authorization = "Bearer $($login.data.token)" }
} finally {
    $password = $null
    $loginBody = $null
    $login = $null
}

try {
    # 唯一 slug 便于重复练习；owner 由登录身份决定，不放进请求
    $slug = 'learn-' + [Guid]::NewGuid().ToString('N').Substring(0, 12)
    $createBody = @{ slug = $slug; title = '学习请求链'; content = '第一版正文' } |
        ConvertTo-Json -Compress
    $created = Invoke-RestMethod -Method Post -Uri "$base/api/v1/examples/notes" `
        -Headers $headers -ContentType 'application/json; charset=utf-8' -Body $createBody
    $noteID = $created.data.id
    $revision = $created.data.revision

    # 列表和详情
    $page = Invoke-RestMethod -Uri "$base/api/v1/examples/notes?page=1&pageSize=20" -Headers $headers
    $page.data.items | Format-Table id, slug, title, revision
    $detail = Invoke-RestMethod -Uri "$base/api/v1/examples/notes/$noteID" -Headers $headers
    $detail.data | Select-Object id, title, revision

    # 第一次更新成功，版本从 1 变为 2；空 content 是主动清空
    $updateBody = @{ title = '已理解 controller 到 mapper'; content = ''; revision = $revision } |
        ConvertTo-Json -Compress
    $updated = Invoke-RestMethod -Method Put -Uri "$base/api/v1/examples/notes/$noteID" `
        -Headers $headers -ContentType 'application/json; charset=utf-8' -Body $updateBody
    $updated.data | Select-Object id, title, revision

    # 原请求仍拿旧 revision，必须得到 409，不能覆盖刚才的修改
    $conflict = Invoke-WebRequest -Method Put -Uri "$base/api/v1/examples/notes/$noteID" `
        -Headers $headers -ContentType 'application/json; charset=utf-8' -Body $updateBody `
        -SkipHttpErrorCheck
    if ([int]$conflict.StatusCode -ne 409) { throw '预期收到版本冲突 409' }
    ($conflict.Content | ConvertFrom-Json).error | Format-List code, message

    # 删除必须传最新版本；随后查询应为 404
    $deleteRevision = $updated.data.revision
    $deleted = Invoke-RestMethod -Method Delete `
        -Uri "$base/api/v1/examples/notes/${noteID}?revision=$deleteRevision" -Headers $headers
    if (-not $deleted.data.deleted) { throw '删除未确认成功' }
    $missing = Invoke-WebRequest -Uri "$base/api/v1/examples/notes/$noteID" `
        -Headers $headers -SkipHttpErrorCheck
    if ([int]$missing.StatusCode -ne 404) { throw '删除后预期收到 404' }
} finally {
    if ($null -ne $headers) {
        try {
            Invoke-RestMethod -Method Post -Uri "$base/api/v1/auth/logout" -Headers $headers | Out-Null
        } finally {
            $headers.Clear()
            $headers = $null
        }
    }
}
```

若先测重启持久化，可在删除之前暂停：停止并重启示例，重新登录，再用已记录的笔记 ID 查询。旧 token 随进程退出失效，笔记保留在数据库。启用副本后，List 可以短暂看不到刚写入的数据，Get 详情固定主库，仍能立即确认创建结果。

## 4. 按断点顺序读源码

### 第一步：公共入口与两种装配

看 [cmd.Run](../cmd/gin_server.go)：默认选 wire.NewContainer，`-di wire` 选 NewContainerWithWire，两者接收相同的 ctx、cfg、bootstrap.Options。Options 的 EnableExamples 决定集中模型与路由是否包含示例；InitializeSchema 由 migrate 设置。

[manual.go](../pkg/wire/manual.go) 直接按 `mapper.NewNoteMapperWithReader → impl.NewNoteService → controller.NewNoteController` 构造业务对象。只有 Note.List 使用 Cluster.Read，其余操作固定主库；事务使用 NewNoteMapper(tx)。可选生成声明集中在 [providers.go](../pkg/wire/providers.go)，所有层只有一张 Wire 图。构造函数不查表、不迁移、不启动后台任务。

Container 公共 finish 调用 Prepare，先处理 [集中模型列表](../internal/model/get_all_models.go)，再补缺失账号/配置。成功之后 CLI 才调用 `router.New(container)`，最后创建 Runtime。不要把首次初始化推迟到 handler 中。

### 第二步：路由声明路径与权限

看 [router/router.go](../pkg/router/router.go) 和 [routes/note_routes.go](../pkg/router/routes/note_routes.go)。router 统一 `/api/v1`、认证、中间件与示例开关；资源文件把 `/examples/notes` 的方法绑定到 handler，并分别声明 NoteReadPermission、NoteWritePermission。

401 表示无有效登录，403 表示无操作权限；即使有读写权限，仍必须把 owner 传到 SQL 保证资源隔离。router 不构造服务、不处理正文和 SQL。

### 第三步：Controller、DTO 和 VO

在 [note_controller.go](../pkg/serve/controller/note_controller.go) 的 Create 下断点：ownerID 读取可信 Principal → BindJSON 解析 [CreateNoteRequest](../pkg/serve/controller/dto/note_dto.go) → 转 service.CreateNoteInput，传请求 ctx 和 owner → errors.Is 分类 → [vo.FromNoteModel](../pkg/vo/note_vo.go)以 201 响应。

PUT 的 title/content 指针区分缺字段与主动空字符串，revision 必须为正数；slug 和 owner 不可由更新请求修改。数据库模型不直接序列化，VO 显式列出公开字段。

### 第四步：Service 契约与 impl 规则

[service/note_service.go](../pkg/serve/service/note_service.go) 定义 NoteService、NoteMapper、CreateNoteInput、UpdateNoteInput、NotePage，不依赖 Gin/GORM。[impl/note_service_impl.go](../pkg/serve/service/impl/note_service_impl.go)执行真实校验：slug 规则、标题修剪、字符长度、UTF-8/NUL、有效 ID、分页、revision 范围。

服务调用者即使是任务或命令行，也会执行同样校验。测试用窄 mapper 替身检查非法输入不会落库；不是把规则全部放在 DTO 标签上。

### 第五步：Mapper 与数据库约束

看 [note_mapper.go](../pkg/serve/mapper/note_mapper.go) 和 [model/note.go](../internal/model/note/note.go)。所有 I/O 用 WithContext，Get/List/Update/Delete 的 SQL 带 owner；(owner_id,slug) 唯一约束处理并发重复创建。

PostgreSQL Update 的逻辑为：

```sql
UPDATE example_notes
SET title = $1, content = $2, revision = revision + 1, updated_at = $3
WHERE owner_id = $4 AND id = $5 AND revision = $6
RETURNING *;
```

比较与修改在同一 SQL 完成，同 revision 并发只有一个成功。map 更新支持主动清空正文；PostgreSQL RETURNING 返回本次写入行，MySQL 在同一事务 UPDATE 后带锁回读。RowsAffected 为零时固定主库，按当前 owner 范围区分不存在与冲突。Delete 同样带 revision。safeError 将原始驱动错误脱敏成稳定类别，保留 ctx 取消分类。

单条 CAS 无需额外事务；多次写入一起提交的完整代码见 [事务传播](EXTENDING.md#4-事务如何传播)。

### 第六步：错误、日志与停机

Controller 将参数/未找到/冲突/故障映射为 400/404/409/503，错误码固定，不回显正文或 SQL。访问日志只记录注册路径与安全状态，requestId 对应响应头和 envelope。

Runtime 取消流后，HTTP 与 WebSocket 共用单次 ShutdownTimeout 排空；Close 不追加等待预算，最终 Streams → Auth → DB。业务 I/O 必须响应 ctx。

## 5. 修改之后怎么验证

| 文件 | 验证重点 |
|---|---|
| [note_service_impl_test.go](../pkg/serve/service/impl/note_service_impl_test.go) | 标题/slug/身份/revision 规则，非法输入不调用 mapper |
| [note_mapper_test.go](../pkg/serve/mapper/note_mapper_test.go) | 真实 SQL、唯一约束、owner、清空正文、CAS 并发和取消 |
| [note_routes_test.go](../pkg/router/routes/note_routes_test.go) | HTTP、权限、DTO/VO、归属、分页与版本冲突 |
| [wire_test.go](../pkg/wire/wire_test.go) | 默认手写与可选生成装配、初始化、失败清理 |

```powershell
go test ./pkg/serve/service/impl ./internal/model/note -count=1
go generate ./pkg/wire
go tool wire diff ./pkg/wire
go vet ./...
go test ./pkg/serve/mapper ./pkg/router/... ./pkg/wire -count=1
go build -o .build/heago-server .
go build -o .build/heago-example ./cmd/example
```

数据库测试需要 `HEAGO_TEST_DATABASE_URL`（PostgreSQL）或 `HEAGO_TEST_MYSQL_DSN`（MySQL）。两个驱动分别测试，同时设置时优先 PostgreSQL；都未设置会 skip，不能视为数据库验证通过。使用本模板默认 PostgreSQL 开发数据库时，可在终端按提示输入其密码，避免写入命令历史：

```powershell
$databasePassword = Read-Host '本模板 PostgreSQL 密码' -MaskInput
$escapedPassword = [uri]::EscapeDataString($databasePassword)
$env:HEAGO_TEST_DATABASE_URL = "postgres://heago:$escapedPassword@127.0.0.1:55434/heago?sslmode=disable"
try {
    go test ./pkg/serve/mapper ./pkg/router/... ./pkg/wire -count=1
} finally {
    Remove-Item Env:HEAGO_TEST_DATABASE_URL
    $databasePassword = $null
    $escapedPassword = $null
}
```

PostgreSQL 测试创建并清理随机 schema，MySQL 使用随机 database；不要把线上连接交给测试。MySQL DSN 填写在私有测试环境，格式为 `user:password@tcp(host:3306)/database?parseTime=true&tls=false`，勿把真实值写进历史或文档。复制延迟、回退和节点停机的验收见 [数据库手册](DATABASE.md)；Linux race 及已实际执行结果见 [验证记录](VERIFICATION.md)。能完成登录到 CRUD、解释 owner/revision 的作用并通过边界验证，即可按 [扩展手册](EXTENDING.md)增加自己的资源。

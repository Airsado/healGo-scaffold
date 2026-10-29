# 真实主从复制验收环境

## 职责

用独立的 PostgreSQL 16 和 MySQL 8.4 主从环境验证数据库路由、复制延迟、副本只读约束及节点故障。它是本地测试工具；生产数据库仍由数据库管理员或云平台提供复制、备份及主库切换。

## 内容

- [replication.ps1](replication.ps1)：PowerShell 7 主脚本，通过 Docker 启动、暂停复制、恢复、查看状态和清理测试资源。
- [run-tests.ps1](run-tests.ps1)：加载私有连接变量，依次运行 PostgreSQL/MySQL 全量测试；可选执行会改变副本状态的复制验收。
- PostgreSQL 使用 `pg_basebackup` 建立物理流复制；副本回放可以暂停，应用读账号只有查询权限。
- MySQL 使用 GTID 自动定位；副本开启并持久化 `read_only`、`super_read_only`，应用读账号只有 `SELECT`、`SHOW VIEW`。
- 每次 `Start` 在主库写入新的随机探针值，并等待副本读到该值，避免把旧数据当成复制成功。

所有资源的名称以 `heago-rw-test-` 开头且带有 `heago.replication-test=true` 标签。脚本遇到同名但标签不匹配的资源会停止，不操作其他项目的数据库。

| 数据库 | 主库 | 副本 | 数据库名 |
|---|---|---|---|
| PostgreSQL | `127.0.0.1:55435` | `127.0.0.1:55436` | `heago` |
| MySQL | `127.0.0.1:53316` | `127.0.0.1:53317` | `heago` |

## 使用

在模板根目录执行，要求 PowerShell 7、Docker Linux 容器以及上述四个空闲端口，无须宿主机安装 `psql` 或 `mysql`。

```powershell
# 默认启动两种数据库，首次会下载镜像，后续复用数据和密码
pwsh -NoProfile -File scripts/replication/replication.ps1 -Action Start
pwsh -NoProfile -File scripts/replication/replication.ps1 -Action Status

# 仅暂停副本回放，主库继续接受写入；用于复现“写入成功但副本尚未看到”
pwsh -NoProfile -File scripts/replication/replication.ps1 -Action Pause -Database Postgres
pwsh -NoProfile -File scripts/replication/replication.ps1 -Action Resume -Database Postgres
pwsh -NoProfile -File scripts/replication/replication.ps1 -Action Pause -Database MySQL
pwsh -NoProfile -File scripts/replication/replication.ps1 -Action Resume -Database MySQL

# 故障注入只操作选定的副本，不停止主库
pwsh -NoProfile -File scripts/replication/replication.ps1 -Action StopReplica -Database Postgres
pwsh -NoProfile -File scripts/replication/replication.ps1 -Action StartReplica -Database Postgres

# 停止保留数据；Start 会恢复节点和复制
pwsh -NoProfile -File scripts/replication/replication.ps1 -Action Stop
pwsh -NoProfile -File scripts/replication/replication.ps1 -Action Start

# 删除本测试环境的容器、数据卷与网络；不删除私有凭据文件
pwsh -NoProfile -File scripts/replication/replication.ps1 -Action Clean
```

`-Database Postgres` 或 `-Database MySQL` 可限定操作范围；默认 `All`。`Start` 会恢复之前暂停的复制。停机、暂停及清理前应确认其他测试没有使用这些节点；不要在并行测试期间改变其状态。

推荐的完整验收入口如下。脚本每次只启用当前单库测试变量，最后恢复调用者原有进程环境。`-IncludeReplication` 会显式开启 `HEAGO_TEST_REPLICATION=1`，运行 `TestReplicationIntegration`，其中暂停复制、停止副本属于该测试的故障注入步骤；其他测试不会自动执行这些操作。

```powershell
pwsh -NoProfile -File scripts/replication/run-tests.ps1
pwsh -NoProfile -File scripts/replication/run-tests.ps1 -Database MySQL
pwsh -NoProfile -File scripts/replication/run-tests.ps1 -IncludeReplication
# Linux/已配置C编译器的Windows可额外加 -Race
```

## 私有文件与测试变量

脚本在被 Git 忽略的 `.local/replication-test/` 下生成：

- `credentials.json`：管理员、写账号、只读账号及复制账号的随机密码。
- `postgres-primary.env`、`postgres-replica.env`、`mysql.env`：Docker 启动或首次备份所需环境变量。
- `test.env`：Go 测试使用的连接变量。
- `state.json`：结构化的 `postgres.primary`、`postgres.replica`、`mysql.primary`、`mysql.replica` 节点信息，字段是 `host`、`port`、`name`、`user`、`password`，PostgreSQL 另有 `sslmode`，MySQL 另有仅用于本地测试的 `tls: "false"`。

这些文件包含秘密信息；不要打印、提交或贴入工单。普通账号密码为 192 bit 随机值；MySQL 复制密码因协议最长 32 字符限制，使用 128 bit 随机值。脚本输出和异常会遮蔽已加载密码。

`test.env` 的变量如下。前四个变量用真实写账号/只读账号，后四个供需要创建测试 schema/database 的套件使用，管理员仅限这个可销毁环境。

| 变量 | 用途 |
|---|---|
| `HEAGO_TEST_POSTGRES_PRIMARY_DSN` | PostgreSQL 主库应用连接 |
| `HEAGO_TEST_POSTGRES_REPLICA_DSN` | PostgreSQL 副本只读连接 |
| `HEAGO_TEST_MYSQL_PRIMARY_DSN` | MySQL 主库应用连接 |
| `HEAGO_TEST_MYSQL_REPLICA_DSN` | MySQL 副本只读连接 |
| `HEAGO_TEST_POSTGRES_ADMIN_DSN` | PostgreSQL 主库测试管理连接 |
| `HEAGO_TEST_MYSQL_ADMIN_DSN` | MySQL 主库测试管理连接 |
| `HEAGO_TEST_DATABASE_URL` | 现有 PostgreSQL 测试变量，与 PostgreSQL 管理连接相同 |
| `HEAGO_TEST_MYSQL_DSN` | MySQL 随机数据库测试变量，与 MySQL 管理连接相同 |

以下加载方式不会显示连接字符串，也不会执行环境文件中的代码；测试后删除本次设置的进程环境变量：

```powershell
$testVariables = @()
try {
    foreach ($line in Get-Content -LiteralPath .local/replication-test/test.env) {
        if ([string]::IsNullOrWhiteSpace($line)) { continue }
        $pair = $line.Split('=', 2)
        [Environment]::SetEnvironmentVariable($pair[0], $pair[1], 'Process')
        $testVariables += $pair[0]
    }
    go test -count=1 ./... -timeout 240s
    if ($LASTEXITCODE -ne 0) { throw 'Go tests failed.' }
} finally {
    foreach ($name in $testVariables) {
        [Environment]::SetEnvironmentVariable($name, $null, 'Process')
    }
}
```

环境文件始终包含两种数据库的变量，因此加载全部变量前应默认 `Start` 两种数据库；如果只启动一种，则只加载对应变量。不要把测试管理员凭据复制到部署配置。

## 约定与限制

复制是异步的，副本落后不会自动变成错误。暂停命令让 PostgreSQL 停止 WAL 回放、MySQL 停止 SQL 线程，因此网络连接仍健康但数据保持旧版本，适合验证业务显式读取主库的规则。

这套工具只负责建立可复现的单主单副本环境，不包含自动主库选举、提升副本、跨数据库类型复制、TLS 生产配置、备份恢复或高可用编排。副本账号的只读权限是额外保护，不能代替应用层的事务和一致性约束。

脚本只绑定本机回环地址。`Clean` 仅删除精确命名且标签匹配的 Docker 资源；`.local/replication-test/` 中的文件会保留供排查及重复运行。若首次 `pg_basebackup` 中途失败留下不完整数据，先用 `Clean -Database Postgres` 清理该测试数据库，再 `Start -Database Postgres`，不要直接修改其数据目录。

## 职责

读取进程启动前必须知道的文件配置：HTTP、数据库拓扑、认证期限、初始管理员及加密主密钥。可在后台编辑的运行配置由数据库保存，不在此增加第二份来源。

## 内容

[config.go](config.go) 提供默认值、严格 YAML 解析与校验；[database.go](database.go) 负责 PostgreSQL/MySQL 及主副本配置。选择一个文件复制为私有配置：

| 模板 | 用途 |
| --- | --- |
| [config.example.yaml](config.example.yaml) | PostgreSQL 单库，兼容原有平铺连接字段 |
| [config.mysql.example.yaml](config.mysql.example.yaml) | MySQL 8.4 LTS 单库 |
| [config.replication.example.yaml](config.replication.example.yaml) | 单主单副本，示例采用 PostgreSQL |

`config_test.go`、`database_test.go` 覆盖严格解析、驱动默认值、主副本继承、无效拓扑和脱敏。所有示例都有占位凭据，不能直接启动服务。

## 约定

由启动层调用 `Load`，校验成功后再创建日志和数据库。私有配置不提交；修改初始密码不会重置已有账号。仅接受一个 YAML 文档，错误不回显配置值。

`database.driver` 仅接受 `postgres`（默认）或 `mysql`；一个拓扑只使用一种驱动和同一个数据库名。PostgreSQL 默认端口 `5432`、`ssl_mode: require`，MySQL 默认端口 `3306`、`tls: true`。MySQL 的 `tls` 仅接受 `true`/`false`，不接受跳过证书校验的模式。生产部署应使用可信证书；示例关闭 TLS 仅用于本地/私有网络。

单库保留 `database.host/port/name/user/password` 的原有写法。采用 `database.primary` 后，不得同时提供这些平铺连接字段，也不得平铺 `ssl_mode/tls`；`max_open` 等池参数仍可作为公共默认。主库端点可以覆盖公共池参数。副本继承主库的端口、数据库名、TLS、池参数与连接超时，但必须单独提供 `host/user/password`。空密码不会继承主库密码。副本的数据库名必须等于主库；第一版最多一个副本。

副本应使用数据库层面限制的只读账号；配置只能明确连接身份，无法替运维创建账号或部署数据库复制。`fallback_to_primary` 默认 `true`，只控制允许副本读取的用例能否回主库；认证和写事务等强一致读取仍固定主库。

代码接入使用 `Database.PrimaryConfig()` 和 `Database.ReplicaConfigs()` 获取无拓扑的连接值，`EffectiveDriver()` 对旧结构返回 `postgres`。`Normalize()` 只归一化驱动/节点，不修改凭据。直接在 Go 中构造配置时，布尔零值 `FallbackToPrimary: false` 表示关闭回退；YAML 省略时由 `Load` 提供 `true`。

在项目根目录执行 `go test ./configs` 验证。初始化文件使用根目录的 `scripts/init-config.ps1` 或 `scripts/init-config.sh`。

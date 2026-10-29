## 职责

管理持久化账号、角色、权限，并适配 Sa-Token-Go 的随机会话。认证确认“是谁”，角色或权限检查决定“能做什么”，业务不另建 JWT 或直接读取密码哈希。

## 内容

[models.go](models.go) 定义系统表和公开身份；[repository.go](repository.go) 处理事务、初始账号和查询；[service.go](service.go) 提供登录、撤销、改密与 RBAC。[service_test.go](service_test.go) 验证真实数据库行为，[transaction_test.go](transaction_test.go) 验证独立连接池间互斥、等待取消与已获锁事务取消后的释放。

## 约定

组合根先构造服务，构造器不查询系统表；Prepare 建表或检查表后才执行 Bootstrap。停机由 Wire 聚合 cleanup 调用 Close。仅接收 Bearer 请求头；密码使用 bcrypt。数据库保留身份，当前内存会话重启失效，部署限定单应用实例。调用者传递 context，不记录密码或 token。

服务始终接收普通主库连接。登录、凭据版本和权限检查必须读到最新提交，不经过副本；复制延迟不能让已禁用账号继续获得权限。初始化和最后管理员变更通过 [数据库锁事务](../db/transaction.go) 跨进程串行：PostgreSQL 使用事务锁，MySQL 使用覆盖提交的同连接会话锁。MySQL 数据表使用 `utf8mb4_bin`，使 `Alice` 和 `alice` 的大小写语义与 PostgreSQL 一致。

设置 `HEAGO_TEST_DATABASE_URL`（PostgreSQL）或 `HEAGO_TEST_MYSQL_DSN`（MySQL）后，在根目录执行 `go test ./internal/auth -count=1`。两个变量都有值时优先 PostgreSQL，CI 应分两次运行。测试数据库账号需要创建和清理隔离 schema/database 的权限，仅用于专用测试实例。

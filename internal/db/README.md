## 职责

建立 PostgreSQL/MySQL 8 连接、管理主副本连接池并提供显式 GORM 表初始化入口。这里只处理数据库基础设施，不定义业务模型，不负责创建数据库实例、账号、复制关系或执行备份恢复。

## 内容

[db.go](db.go) 提供单连接 `Open`、`Ping`、`Close`、`Initialize` 和 `CheckSchema`；[cluster.go](cluster.go) 提供 `OpenCluster`、`Cluster.Primary`、显式 `Read` 及统一 `Close`；[transaction.go](transaction.go) 处理驱动对应的跨进程互斥事务与迁移。测试覆盖凭据转义、取消重试、安全日志、隔离初始化与读取路由。

## 约定

组合根持有 Cluster 并统一关闭全部连接。Primary 是不自动分流的普通 GORM 句柄；默认所有读取走主库，只有 `Read(ctx,fn)` 显式允许副本。回调只做可安全重试的读取、返回原始错误，结束后再在 Mapper 边界脱敏。每次尝试传递同一个请求 ctx；启用回退时，仅连接错误在剩余期限内回主库一次。

启动严格检查所有已配置节点；运行中副本故障的回退不会改变写入、详情、认证及就绪检查固定主库的规则。事务使用 `Primary.WithContext(ctx).Transaction(...)`，所有参与 Mapper 重新绑定同一 tx，不能使用带副本 reader 的原实例。

重试次数与单次时间均受限；SQL 和原始驱动错误不写日志。PostgreSQL 初始化在事务锁内执行；MySQL 使用同连接会话锁，DDL 隐式提交不能承诺整体回滚。默认启动仅检查表存在，不等于完整版本迁移校验。详细配置与示例见 [数据库手册](../../docs/DATABASE.md)。

设置 `HEAGO_TEST_DATABASE_URL` 或 `HEAGO_TEST_MYSQL_DSN`，在根目录执行 `go test ./internal/db -count=1`。两个驱动分开运行，只使用专用测试实例并清理随机 schema/database。

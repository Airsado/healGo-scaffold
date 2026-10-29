## 职责

为真实数据库集成测试创建独立环境。返回的 GORM 连接只属于当前测试，测试结束后关闭连接并删除隔离的 schema 或 database。

## 内容

[database.go](database.go) 的 `OpenDatabase(t)` 优先读取 `HEAGO_TEST_DATABASE_URL`（PostgreSQL），否则读取 `HEAGO_TEST_MYSQL_DSN`（MySQL）；都未设置时跳过集成测试。调用包显式登记自己的模型并执行迁移。

## 约定

仅测试代码可以调用，生产启动不得引用。DSN 来自本地私有环境，错误不输出账号密码。测试账号需要创建、删除 PostgreSQL schema 或 MySQL database 的权限；只用于专用测试实例。每次名称由固定前缀和随机十六进制组成，不接受外部 DDL 标识符。MySQL 使用 InnoDB 和 utf8mb4_bin，保持事务与大小写语义。两个数据库的 CI 分开运行，避免同时设置环境变量导致只验证 PostgreSQL。
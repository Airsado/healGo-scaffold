## 职责

将已注册的运行配置持久化到 PostgreSQL 或 MySQL，保持类型、版本与审计一致。数据库连接凭据和加密主密钥仍来自启动文件；此处不是任意键值写入接口，也不包含预置业务配置。

## 内容

[models.go](models.go) 定义注册项、公开视图和系统表；[validation.go](validation.go) 校验 JSON 类型与规则；[crypto.go](crypto.go) 实现 AES-GCM；[service.go](service.go) 提供初始化、读取、修改及审计。

## 约定

构造服务只校验定义和密钥，不查询表；Prepare 集中初始化模型后才调用 Initialize。默认值只补缺失行，读取不缓存。秘密不回显，更新凭 revision 防止覆盖；改值与审计同事务。业务内部取值使用 `Value`，不得记录解密内容；更换主密钥前必须另行迁移密文。

服务始终注入普通主库连接，配置更新后的回读及凭据读取不经过副本。`key` 字段通过 GORM 列表达式引用，由驱动生成引号，兼容 MySQL 保留字。载荷保持原始二进制格式：PostgreSQL 使用 `bytea`，MySQL 使用 `longblob`。普通 `BLOB` 的 65535 字节不足以容纳 64 KiB JSON 加 AES-GCM 的 nonce/tag，因此不能直接替换。

设置 `HEAGO_TEST_DATABASE_URL`（PostgreSQL）或 `HEAGO_TEST_MYSQL_DSN`（MySQL）后，在根目录执行 `go test ./internal/settings -count=1`。同时设置时优先 PostgreSQL，两个驱动应分别验收。真实数据库测试覆盖并发初始化、CAS、审计失败回滚及最大加密载荷；账号需能创建和清理隔离 schema/database，仅用于专用测试实例。

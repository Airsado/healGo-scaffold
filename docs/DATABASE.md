# 数据库、读写分离与一致性

模板支持 PostgreSQL 和 MySQL 8（推荐 MySQL 8.4 LTS）。默认只配置一个主库，所有操作仍走主库；配置一个副本后，只把显式允许复制延迟的读取送到副本。MVC 目录及 Service 接口不因数据库数量变化。

## 1. 先选配置方式

| 场景 | 文件模板 | 说明 |
| --- | --- | --- |
| PostgreSQL 单库 | [config.example.yaml](../configs/config.example.yaml) | 兼容旧配置，省略 driver 仍使用 PostgreSQL |
| MySQL 单库 | [config.mysql.example.yaml](../configs/config.mysql.example.yaml) | 默认 3306，`tls: true`；示例为本地网络关闭 TLS |
| 单主单副本 | [config.replication.example.yaml](../configs/config.replication.example.yaml) | 使用 primary/replicas；默认允许受控读取回主库 |

复制一个模板到 Git 忽略的 `configs/config.local.yaml`，填写真实凭据和加密密钥。数据库主副本地址、账号和密码都是启动文件配置，修改后重启；运行配置仍由 `internal/settings` 持久化。示例占位密码无法通过启动校验。

已有 PostgreSQL 的 `scripts/init-config.*` 和 Compose 默认部署保留。MySQL 或已有主副本集群使用相应模板连接；切换 driver 不会迁移原 PostgreSQL 的数据。运行 `migrate` 前，数据库实例、数据库及账号应由部署侧创建。

主副本示意（省略 HTTP、密钥等其他必填配置）：

```yaml
database:
  driver: mysql
  max_open: 20
  max_idle: 5
  fallback_to_primary: true
  primary:
    host: mysql-primary
    name: heago
    user: heago_writer
    password: "REPLACE_WITH_PRIMARY_PASSWORD"
    tls: false
  replicas:
    - host: mysql-replica
      user: heago_reader
      password: "REPLACE_WITH_REPLICA_PASSWORD"
      max_open: 10
      max_idle: 2
```

一期最多一个副本；整个拓扑只使用一种 driver 和同一个数据库名。副本省略的端口、数据库名、TLS、连接超时及池参数继承主库；host/user/password 必须独立提供，密码不会继承。每个节点有自己的池，总连接上限是各池 `max_open` 的总和。

使用 `primary` 时，不可同时平铺 host/port/name/user/password/ssl_mode/tls。公共池参数和重试配置仍放 database 下。PostgreSQL 用 `ssl_mode`，MySQL 用 `tls: true/false`；MySQL 不接受 `skip-verify`。生产证书应由操作系统信任，不能用跳过验证规避证书问题。

副本需要数据库强制只读的账号。应用连接分流不负责配置复制、账号权限、主库选举或数据库故障切换。

## 2. 哪些读取允许走副本

| 操作 | 连接 | 一致性要求 |
| --- | --- | --- |
| 登录、凭据版本、权限及账号状态检查 | 主库 | 立即看到禁用、改密与撤权 |
| 持久化配置读写、密钥取值 | 主库 | 避免读到旧配置或旧密钥 |
| 新增、更新、删除及其结果回读 | 主库 | 返回本次已提交结果 |
| 笔记详情 Get、版本冲突/不存在判断 | 主库 | 写后立即可见，正确区分 409/404 |
| 事务、行锁、迁移与就绪检查 | 主库 | 固定同一数据库；就绪检查确认主库连接 |
| 笔记 List、允许延迟的历史列表 | 显式副本读取 | 可以暂时少一条或显示旧内容 |

新业务默认主库。Service 定义用例是否接受延迟；Mapper 选择具体连接。Controller 不接受客户端随意指定主副本，不把数据库节点加入公开协议。

创建成功后查列表，副本可能尚未复制到新数据；查详情仍走主库。副本返回“没有数据”是正常查询结果，不触发回主库。不要用自动回退掩盖复制延迟。

## 3. 显式注入与代码写法

[OpenCluster](../internal/db/cluster.go) 创建 `*db.Cluster`，其 `Primary` 是普通 `*gorm.DB`。默认主库句柄没有自动分流插件，所以现有 `SELECT` 不会意外进入副本。`Cluster.Close()` 统一关闭主副本池，不能只关闭 Primary。

组合根的示例构造入口是：

```go
// 普通 CRUD 的主库连接固定；只有 List 显式使用 cluster.Read
noteMapper, err := mapper.NewNoteMapperWithReader(cluster)
if err != nil { return nil, nil, err }
noteService := impl.NewNoteService(noteMapper)
noteController := controller.NewNoteController(noteService)
```

默认 Wire 在 [providers.go](../pkg/wire/providers.go) 声明 mapper provider，可选手写入口在 [manual.go](../pkg/wire/manual.go)。服务实现仍只接收 `service.NoteMapper` 接口。自己构造 `mapper.NewNoteMapper(cluster.Primary)` 时，所有方法都固定主库。

新增允许延迟的查询，使用 `Cluster.Read(ctx, fn)`；以下是可放在当前 module 的完整示例：

```go
package query

import (
    "context"
    "errors"

    "healgo-scaffold/internal/db"
    model "healgo-scaffold/internal/model/note"
    "healgo-scaffold/pkg/serve/service"
    "gorm.io/gorm"
)

func NoteTitles(ctx context.Context, cluster *db.Cluster, ownerID uint64) ([]string, error) {
    var titles []string
    err := cluster.Read(ctx, func(database *gorm.DB) error {
        // 连接故障时可能重试一次，因此先清空上一次尝试的局部结果
        titles = make([]string, 0)
        return database.Model(&model.Note{}).Where("owner_id = ?", ownerID).
            Order("id DESC").Limit(20).Pluck("title", &titles).Error
    })
    if err != nil {
        if ctx.Err() != nil {
            return nil, errors.Join(service.ErrUnavailable, ctx.Err())
        }
        return nil, service.ErrUnavailable
    }
    return titles, nil
}
```

回调只做安全可重试的读取，不能写库、发消息或修改外部状态，也不能把原始驱动错误提前改成业务错误；回退判断需要原始连接错误。最终错误在 Mapper 边界分类、脱敏。每次尝试使用同一个请求 ctx，不延长原请求期限。

一个分页请求的 Count 和 Find 放在同一个回调，以便两次 SQL 使用同一节点。它们仍可能看到相邻时刻的数据；要求严格快照时应显式设计只读事务及隔离级别。

## 4. 事务固定主库和 tx

普通多表写用例从 `cluster.Primary.WithContext(ctx).Transaction(...)` 开始，闭包内对所有参与者调用 `mapper.NewNoteMapper(tx)`。不要使用带 cluster reader 的旧 Mapper，也不要中途从 Container 重新取普通 DB。

```go
err := cluster.Primary.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
    txMapper, err := mapper.NewNoteMapper(tx)
    if err != nil { return err }
    notes := impl.NewNoteService(txMapper)
    // 此处所有 Create/Get/List/Update/Delete 均使用这个 tx
    _, err = notes.Create(ctx, ownerID, input)
    return err
})
```

以上为已有函数中的事务片段；包含完整错误分类的可编译多次写入例子见 [扩展手册](EXTENDING.md#4-事务如何传播)。事务必须在成功提交后才能向 HTTP 调用者报告成功。提交失败不自动重放写事务，因为调用方可能不知道服务器是否已经提交。

## 5. 两种驱动的差异

| 项目 | PostgreSQL | MySQL 8 |
| --- | --- | --- |
| 连接默认 | 5432 / ssl_mode=require | 3306 / tls=true |
| 数据库名 / 用户名字节上限 | 63 / 63 | 64 / 32 |
| 二进制加密载荷 | bytea | longblob |
| 大小写 | 默认文本区分大小写 | 初始化使用 utf8mb4 / utf8mb4_bin |
| CAS 返回更新结果 | UPDATE … RETURNING | 同事务 UPDATE 后带锁读取 |
| 跨进程互斥 | 事务 advisory lock | 同连接 GET_LOCK，覆盖提交后释放 |
| 初始化 DDL | 事务内集中迁移 | DDL 可隐式提交，不承诺整体回滚 |

笔记更新仍由 owner/id/revision 的单条条件 UPDATE 决定是否成功，两个驱动并发提交同 revision 都只有一个成功。MySQL 在同一个事务内回读，避免另一个请求插入更新间隙。更新和删除未命中时的分类使用锁定 current read，避免外层可重复读事务的旧快照把已删除记录误报成版本冲突。

`migrate` 只在主库执行。副本表结构通过数据库复制到达；上线时由部署侧确认副本追平。普通启动的表存在检查不证明列/索引完全一致；复杂数据回填、改名、删除及跨版本升级仍需独立迁移流程。

## 6. 故障行为与验证

配置主副本后，启动阶段严格检查所配置节点；任何节点连接失败都不会伪装成完整拓扑启动成功，并清理已经打开的池。副本启动成功后运行中失联，`fallback_to_primary: true` 才允许识别为连接故障的安全读取，在剩余请求期限内回主库一次。未找到、语法错误、权限错误、约束错误、取消和期限耗尽都不靠回退修复。

主库故障时 `/readyz` 失败；写入和强一致查询报不可用。应用不自动提升副本，也不重放结果未知的写事务。副本恢复后，后续显式读取仍可再次尝试副本。

两种数据库测试分别运行：PostgreSQL 使用 `HEAGO_TEST_DATABASE_URL`，MySQL 使用 `HEAGO_TEST_MYSQL_DSN`。两个都提供时测试优先 PostgreSQL，因此不要把同时设置当作双驱动都已验证。测试环境变量只是测试入口，生产启动仍读取 YAML。测试会建立并清理随机 schema/database，必须使用专用测试实例。

除了普通 CRUD，还需验证主副本数据不一致时的分流、写后主库回读、固定事务、副本断开后的受控回退以及主库失联行为。实际运行结果以 [验证记录](VERIFICATION.md) 为准；skip 不能记作通过。

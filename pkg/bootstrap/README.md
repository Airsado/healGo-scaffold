## 职责

集中执行模型准备与缺失种子初始化，并管理 HTTP/流式连接的监听、停机和共享资源释放。对象构造由 [pkg/wire](../wire/README.md)负责，bootstrap 不重复装配业务依赖。

## 内容

[bootstrap.go](bootstrap.go) 定义 Options、Resources、Components、Prepare、DefaultSettings、NewRuntime 与 Runtime。Options 包含 EnableExamples、InitializeSchema、Settings；Resources 保存共享 DB、Logger、Auth、Settings、Streams。

Prepare 从 [集中模型列表](../../internal/model/get_all_models.go)取模型，先初始化或检查 schema，再补缺失账号和配置。默认配置为 system.display_name。CLI 用已初始化的 Container 创建路由后，将 Config、Resources、Router 和 container.Close 交给 NewRuntime。

## 约定

构造函数不查询尚未准备的表；现有账号/配置不被启动默认值覆盖。Components 不拥有资源，Runtime.Close 幂等调用容器清理，顺序为 Streams → Auth → DB。

Serve 先取消流，再排空 HTTP 并等待 WebSocket；http.Server.Shutdown 不追踪 hijack 连接，因此两种连接由同一次 drain 管理，共用单次 ShutdownTimeout 总预算。Close 复用排空结果，不重新计时；未调用 Serve 而直接 Close 也只给予一次预算。先等待 Serve 返回再 Close，业务 I/O 和回调必须响应 ctx，不保存 Gin Context 或遗留后台任务。

相关测试运行 `go test ./pkg/bootstrap ./pkg/wire`，数据库集成需测试 URL；初始化使用 CLI migrate。流边界见 [STREAMING](../../docs/STREAMING.md)。

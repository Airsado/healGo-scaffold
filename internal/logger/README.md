## 职责

根据文件配置构造结构化 slog 日志器，统一等级和输出格式。日志写到标准输出，由 Docker 或部署平台采集、保留和轮转，不在应用内再维护一套文件轮转机制。

## 内容

[logger.go](logger.go) 的 `New` 支持 debug、info、warn、error 及 JSON、文本格式；[logger_test.go](logger_test.go) 检查等级过滤和无效配置拒绝。

## 约定

启动时创建，再通过构造函数传给数据库、路由等组件；不修改全局默认 logger。调用处选择安全字段，例如 requestId、注册路由、状态和耗时，不记录 token、密码或请求正文。数据库日志的 SQL 抑制由 db 包实现。

在项目根目录执行 `go test ./internal/logger`，不需要 PostgreSQL。业务事件使用注入的 logger，不在这里加入具体业务处理。

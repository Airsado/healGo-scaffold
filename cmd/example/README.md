# 独立示例薄入口

## 职责

复用公共 [cmd.Run](../gin_server.go)，以 `EnableExamples` 开启笔记 CRUD、SSE tick 和 WebSocket echo。通用入口默认不登记示例模型或路由。

## 内容

- [main.go](main.go)：薄入口；同样支持 `serve`、`migrate`、`healthcheck` 和 `-di manual|wire`。
- 默认地址 `127.0.0.1:18082`，可用 `-addr` 覆盖，其余配置来自私有 YAML。
- [开发手册](../../docs/DEVELOPMENT.md)提供登录和 CRUD；[流式通信](../../docs/STREAMING.md)提供 SSE/WS、ticket 和代理。

```powershell
go run ./cmd/example migrate -config configs/config.local.yaml
go run ./cmd/example serve -config configs/config.local.yaml -di manual
```

## 约定

migrate 和 serve 必须开启同样的示例；migrate 创建 `example_notes`，realtime 不创建表。认证、设置、连接池和关闭流程复用框架。学习完成后新增真实业务到固定目录，无需维护第二套框架或复制整套入口。Ctrl+C 走公共优雅停机。

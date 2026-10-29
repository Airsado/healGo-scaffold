# 命令入口

## 职责

集中处理命令、YAML、注入方式、进程信号与退出，不在 main 中编写业务规则或 SQL。

## 内容

- [gin_server.go](gin_server.go)：公共 `Run(args []string, enableExamples bool) error`，提供 `serve`、`migrate`、`healthcheck`。
- [根 main.go](../main.go)：通用服务入口，调用 Run 并关闭示例；从项目根目录执行 `go run .`。
- [example/main.go](example/main.go)：调用同一 Run，开启示例，默认地址 `127.0.0.1:18082`。
- `-config` 指定私有 YAML，`-di wire|manual` 选择装配，默认 wire；`-addr` 覆盖监听地址。

项目只保留通用根入口与独立示例两个程序。开发与构建命令在项目根目录执行：

```powershell
go run . migrate -config configs/config.local.yaml
go run . serve -config configs/config.local.yaml
go build -o .build/healgo-server .
go build -o .build/healgo-example ./cmd/example
```

独立示例的运行命令是 `go run ./cmd/example migrate` 和 `go run ./cmd/example serve`，同样使用 `-config`。两种程序默认都使用 `wire.NewContainer`；`-di manual` 显式选择 `NewContainerManual`。

## 约定

先构造 Container，再通过 `router.NewEngine(container)` 设置引擎、`router.New(engine, container)` 登记路由，最后创建 Runtime。健康检查只请求 `/readyz`，不打开数据库池。示例迁移和启动必须使用相同开关；初始化不重置已有密码或配置。错误不打印配置、DSN、密码或 token；中断和 SIGTERM 走统一停机。

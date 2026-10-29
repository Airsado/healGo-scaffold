# WebSocket

## 职责

为显式选择 WebSocket 的 HTTP 端点提供连接登记、来源校验、定期授权、心跳、消息限制和取消边界。它复用 `internal/stream.Manager` 的应用实例总容量，服务停止时关闭真正被 hijack 的 TCP 连接。

本包可以供任何业务模块调用，不包含机器人消息、广播队列、订阅状态、重连补偿或数据库模型。普通 HTTP 端点继续使用自己的请求超时。

## 内容

| 文件 | 用途 |
| --- | --- |
| `server.go` | 不可变的服务器选项、握手与授权、心跳、回调与内部任务清理 |
| `conn.go` | 有界文本、二进制、JSON 读写，以及错误分类 |
| `deadline.go` | 清除普通 HTTP 继承的 TCP 期限，兼容 Gin 的延迟响应头提交 |
| `websocket_test.go` | 真实 TCP 连接的协议、来源、权限、心跳、慢读者和停机测试 |

`New(manager, Options{})` 的默认值如下；负数不代表关闭限制，而是配置错误。

| 选项 | 默认值 |
| --- | --- |
| `MaxDuration` | 5 分钟 |
| `WriteTimeout` | 5 秒，包含等待写锁，也用于单次授权检查 |
| `PingInterval` / `PongTimeout` | 20 秒 / 10 秒 |
| `RecheckInterval` | 30 秒 |
| `MaxMessageBytes` | 每条消息 64 KiB，同时限制接收与发送 |
| `AllowedOrigins` | 允许同源或无 Origin 的客户端，额外跨域来源需显式登记 |

文本必须是 UTF-8。`ReadJSON` 只接受文本帧，使用 Go 标准 JSON 解码语义；应用仍应验证字段和业务规则。JSON 编码会先生成内存对象，所以业务需要限制传入对象大小和自定义 `MarshalJSON` 的工作量。WebSocket 压缩默认禁用。

## 约定

1. **一个连接必须持续有一个读取者。** `Read` 不只是接收业务消息，也处理 ping、pong、close；回调只定时写入、从不读取，会在心跳检查时被关闭。多个 goroutine 可以调用 `Write`，同一个连接只允许一个读取循环。
2. **回调及授权函数必须响应 context。** `Serve` 同步执行回调，并在回调与封装内部任务退出后才 `session.End()`。业务创建的 goroutine 也必须由业务取消和等待，不能把工作留给已经结束的请求。
3. **`Serve` 拥有整个响应。** 升级前，本包的权限、来源和容量失败使用统一错误 envelope；底层库的畸形握手校验使用普通 HTTP 错误响应。升级后只关闭连接。无论 `Serve` 返回什么，外层都不能再写 JSON。回调 panic 与授权 panic 会转换为固定错误分类。
4. **来源是完整 origin。** 白名单使用 `https://console.example.com` 这样的精确来源，可接受默认端口等价；禁止通配符，不信任 `X-Forwarded-*`。HTTPS 代理后的公网来源要显式登记。无 Origin 的 CLI 客户端仍然需要认证；来源校验不是身份认证。
5. **`Authorize` 应重新读取当前身份与权限。** 框架在握手前和连接期间检查，不应只检查建立连接时的旧角色。回调收到独立超时 context；忽略它会拖延清理，本包不会遗弃后台 goroutine 来假装已经超时。
6. **长连接有自己的生命周期。** 框架只从 `stream.PreserveRequestContext` 恢复普通 HTTP 预算之前的取消信号，保留后续 context 值；已经取消的请求不能复活。客户端断开、请求取消、授权失效、心跳失败、最长时长和 `Manager.Close()` 都会关闭连接。
7. **本层选择立即关闭 TCP。** `Serve` 结束时不等待最长可达十秒的 close handshake，客户端可能观察到异常关闭码 1006，应按自身策略重连。读写 context 到期也会使底层库关闭连接；不要把失败的 I/O 当成可在同一 socket 上重试的操作。
8. **只记录安全分类。** 返回值不会包含远端 close reason、业务错误原文、消息 payload 或授权错误详情。可用 `errors.Is` 识别 `ErrClosed`、`ErrUnauthorized`、`ErrMessageTooBig`、`ErrInvalidMessage`、`ErrHandler` 和标准 context 错误。

最小回显处理器持续读取，并把返回的 context 传给读写：

```go
err := server.Serve(w, r, func(ctx context.Context, conn *websocket.Conn) error {
    for {
        kind, payload, err := conn.Read(ctx)
        if err != nil { return err }
        if err := conn.Write(ctx, kind, payload); err != nil { return err }
    }
})
// 这里最多记录安全的 err 分类，不能再写 HTTP 响应
```

如果协议主要由服务端发送数据，仍需启动一个读取循环来处理控制帧。给它派生可取消的 context，在发送回调结束时取消并等待读取循环退出。若选择丢弃业务数据，也应明确这是该端点允许的协议行为。

执行 `go test ./internal/stream/websocket`。测试使用 `httptest.Server` 建立真实网络连接，不需要数据库或外部聊天平台。

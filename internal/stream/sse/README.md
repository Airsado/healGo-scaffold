# sse

## 职责

提供通用 Server-Sent Events 协议封装，集中处理事件编码、串行写入、心跳、权限复核与连接取消。业务只生产事件，不手动管理 HTTP Flush 或无限消息队列。

## 内容

| 文件 | 内容 |
|---|---|
| `server.go` | 连接接纳、选项默认值、生命周期与定时任务 |
| `writer.go` | 可取消的串行写、逐次写入超时、JSON 便捷入口 |
| `event.go` | UTF-8 多行编码、字段注入防护、帧大小限制与公开错误分类 |
| `sse_test.go` | 使用真实 HTTP/TCP 连接验证 Flush、断开、超时与停机 |

```go
server, err := sse.New(manager, sse.Options{
    Authorize: func(ctx context.Context) error { return checkCurrentPermission(ctx) },
})
// 检查 err 后，由已完成登录/权限验证的 GET 路由调用：
err = server.Serve(w, r, func(ctx context.Context, out *sse.Writer) error {
    return out.SendJSON("message", map[string]string{"text": "hello"})
})
// Serve 已经负责 HTTP 响应，返回后只记录安全分类，不再写 JSON
```

## 约定

`Options` 的零值使用默认：最长连接 5 分钟、每次写入/Flush 5 秒、心跳 15 秒、权限复核 30 秒、完整事件帧最大 64 KiB。负数、无法容纳空事件的字节限额无效；零值表示默认，不能用零关闭保护。所有流共享 `stream.Manager` 的容量与停机取消。

`Authorize` 在任何 SSE 输出前执行一次，之后周期复核；必须读取当前权限并尊重 context。回调也必须尊重 context、在返回前等待自己的 goroutine；不能在回调退出后继续使用 Writer。管理器关闭、客户端断开、最长时长、写入失败和撤销授权都会取消回调。回调在当前请求 goroutine 执行，框架不会遗留一个失去管理的业务 goroutine。

仅显式进入 Serve 的连接通过 `stream.RequestContext` 恢复 HTTP 请求原始取消信号，从而采用独立流时长；进入时已经过期的普通请求仍被拒绝。其余 API 保留普通请求超时。部署代理也需允许长连接，不能只依赖 `X-Accel-Buffering: no` 覆盖代理的总时长限制。

每个事件同步等待写入与 Flush，只有一个写操作占用响应；不提供重放存储、自动重连游标或发布订阅队列。`ID`/`Name` 禁止控制字符，`Data` 必须是 UTF-8；CRLF、CR、LF 均按多行 data 编码。`Retry` 是正整毫秒，零表示省略。大小限制包含协议字段和换行开销；JSON 调用者仍需限制构造值的规模。

Serve 自行写初始化失败的统一错误信封；流开始后只关闭并返回稳定错误，不向事件体追加 JSON，也不返回原始回调/网络错误。`ResponseWriter` 必须支持 `ResponseController` 的 deadline 与 Flush；Gin 包装器需要暴露 `Unwrap`。框架通过逐次 deadline 和取消时立即 deadline 打断慢写，返回前等待自有心跳与取消 goroutine 退出。

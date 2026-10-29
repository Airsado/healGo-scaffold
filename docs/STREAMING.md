# WebSocket 与 SSE

框架提供传输封装和连接生命周期；realtime 控制器与独立示例入口展示鉴权、浏览器接入和最小消息循环。默认入口不登记示例路由，使用 [cmd/example](../cmd/example/main.go) 运行。

## 1. 启动与路由

先按[开发手册](DEVELOPMENT.md#1-先把示例运行起来)准备私有配置及数据库（默认演示 PostgreSQL，也支持 MySQL），再在项目根目录运行：

```powershell
go run ./cmd/example migrate -config configs/config.local.yaml
go run ./cmd/example serve -config configs/config.local.yaml -addr 127.0.0.1:18082
```

同一示例入口通过 EnableExamples 同时开启笔记与 realtime。Container 构造控制器，router/routes 登记路径；realtime 不登记业务模型。

| 方法与路径 | 认证 | 行为 |
|---|---|---|
| GET `/api/v1/examples/realtime/sse` | Bearer；`example:realtime:connect` | 立即发第一条 tick，随后每秒一条，共五条，再发 done 并结束 |
| POST `/api/v1/examples/realtime/tickets` | Bearer；同上权限 | 201，返回 `{ticket, expires_in: 30}` |
| GET `/api/v1/examples/realtime/ws` | Bearer；同上权限 | 非浏览器客户端升级后原样 echo 文本或二进制消息 |
| GET `/api/v1/examples/realtime/ws?ticket=...` | 单次 ticket，握手时复核原会话和权限 | 原生浏览器 WebSocket 的接入方式 |

初始管理员的 `*` 权限包含连接权限。未登录返回 401，无权限返回 403；ticket 无效、已使用或过期返回 401。普通 bearer token 不接受通过 URL 传入，也不能把 ticket 用在其他 API。

## 2. 按源码找实现

| 文件 | 负责什么 |
|---|---|
| [stream/manager.go](../internal/stream/manager.go) | SSE/WS 共用的连接计数、最大时长、取消和停机等待 |
| [stream/http.go](../internal/stream/http.go) | 保留原始请求取消、统一 prestream 错误、精确 Origin 校验 |
| [stream/sse/server.go](../internal/stream/sse/server.go) | SSE 响应、心跳、周期鉴权与安全退出 |
| [stream/sse/writer.go](../internal/stream/sse/writer.go) | 串行写入、刷新、写超时和事件尺寸限制 |
| [stream/websocket/server.go](../internal/stream/websocket/server.go) | WS 握手、Origin、心跳、鉴权与关闭 |
| [routes/realtime_routes.go](../pkg/router/routes/realtime_routes.go) | SSE/ticket/WS 路径与权限，只有示例开关启用时登记 |
| [controller/realtime_controller.go](../pkg/serve/controller/realtime_controller.go) | SSE 五条 tick、WS echo、每次鉴权同时检查会话与权限 |
| [controller/realtime_tickets.go](../pkg/serve/controller/realtime_tickets.go) | 浏览器握手所需的短期单次凭据 |
| [wire/manual.go](../pkg/wire/manual.go)、[providers.go](../pkg/wire/providers.go) | 手写构造和集中 provider sets，复用同一个控制器构造函数 |

realtime 只演示传输，因此没有空的 model、mapper 或 service。真实聊天、订阅或通知规则按固定 service/impl、mapper、model 等职责接入，控制器只适配协议。

## 3. 通用 API 与边界

两种封装均采用 `New(manager, Options)`，然后调用 `Serve`。回调接收独立的 `context.Context` 和协议对象，不保存 Gin Context。

```go
func serveStatus(manager *stream.Manager, w http.ResponseWriter,
    r *http.Request, authorize func(context.Context) error) {
    server, err := sse.New(manager, sse.Options{Authorize: authorize})
    if err != nil {
        stream.Reject(w, 503, "stream_unavailable", "流连接暂不可用")
        return
    }
    _ = server.Serve(w, r, func(ctx context.Context, out *sse.Writer) error {
        return out.SendJSON("status", map[string]bool{"ready": true})
    })
    // Serve 已处理初始化错误，之后只返回，不能再写 JSON envelope
}
```

WS 的回调使用 `connection.Read(ctx)`、`connection.Write(ctx, kind, payload)`，也提供 `ReadJSON/WriteJSON`。读取循环会驱动 ping/pong/close 控制帧；即使应用只想主动推送，也需要安排受生命周期管理的读取循环，不能只写不读。

| 选项 | 默认值 | 用途 |
|---|---|---|
| 两者 `MaxDuration` | 5 分钟 | 单连接最大寿命 |
| 两者 `WriteTimeout` | 5 秒 | 慢客户端不能永久堵住写入 |
| 两者 `RecheckInterval` | 30 秒 | 重新验证 bearer 会话和当前权限 |
| SSE `HeartbeatInterval` | 15 秒 | 空闲时发送注释心跳 |
| SSE `MaxEventBytes` | 64 KiB | 包括事件格式在内的单事件限制 |
| WS `PingInterval` / `PongTimeout` | 20 秒 / 10 秒 | 探测对端是否仍响应 |
| WS `MaxMessageBytes` | 64 KiB | 单消息读取限制 |

零值选项采用默认值；负数不会关闭保护，而是构造失败。当前通用 provider 创建的 manager 上限为每进程 128 条活跃流，WS/SSE 合计；它与 ticket 存储的 128 条容量是两回事。

`Authorize` 首次在发送 SSE 首帧或 WS 升级之前执行，之后周期复核。示例每次都调用 `Auth.Authenticate` **再**调用 `RequirePermission`，因为只复核权限不能检测已经 logout 或自然到期的 token。撤销到断开存在最长一个复核周期的窗口；需要更短窗口时显式缩短该连接的选项。

普通 API 仍使用原有请求超时。只有显式流封装会恢复原始客户端取消信号并设置自己的最大时长；不会通过全局放宽 HTTP 超时来保活。业务回调必须响应 ctx 取消，且所有内部 I/O 使用该 ctx。

## 4. curl 读取 SSE

将登录接口返回的 token 保留在 shell 内存变量，不打印它。以下为 Bash 示例，`-N` 关闭客户端输出缓冲，`--header @-` 从标准输入读 header，避免将凭据展开在 curl 的进程参数中：

```bash
read -r -s -p '当前登录 token: ' HEAGO_TOKEN
printf '\n'
printf 'Authorization: Bearer %s\n' "$HEAGO_TOKEN" |
  curl -N --header @- http://127.0.0.1:18082/api/v1/examples/realtime/sse
unset HEAGO_TOKEN
```

输出含 `event: tick`、JSON `data`，最后是 `event: done`。注释心跳以 `:` 开头，不是业务事件。示例只有约四秒，因此正常情况下会在首次空闲心跳之前结束。

## 5. 浏览器通过 fetch 读取 SSE

原生 `EventSource` 不能设置 Authorization 请求头。本例使用 `fetch + ReadableStream`，而不是将 token 追加到 URL。下面函数可直接接收登录接口返回、仍在页面内存中的 token；页面须同源，或已被配置到 `security.cors_origins`。

```javascript
async function readTicks(token, signal) {
  const response = await fetch('/api/v1/examples/realtime/sse', {
    headers: { Authorization: `Bearer ${token}` },
    signal,
  });
  if (!response.ok) throw new Error(`SSE 请求失败：${response.status}`);
  if (!response.body) throw new Error('浏览器不支持流式响应');
  const reader = response.body.pipeThrough(new TextDecoderStream()).getReader();
  let pending = '';
  try {
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      pending += value;
      let boundary;
      // 本封装写出 LF 格式；一次网络分块可能含半条或多条事件
      while ((boundary = pending.indexOf('\n\n')) >= 0) {
        const frame = pending.slice(0, boundary);
        pending = pending.slice(boundary + 2);
        let name = 'message';
        const data = [];
        for (const line of frame.split('\n')) {
          if (line.startsWith('event:')) name = line.slice(6).trimStart();
          if (line.startsWith('data:')) data.push(line.slice(5).trimStart());
        }
        if (data.length) {
          console.log(name, JSON.parse(data.join('\n')));
          if (name === 'done') return;
        }
      }
    }
  } finally {
    await reader.cancel().catch(() => {});
    reader.releaseLock();
  }
}

// token 来自已有的登录结果，切勿写成固定凭据
// const stop = new AbortController();
// await readTicks(token, stop.signal);
// 离开页面或主动停止时：stop.abort();
```

这里的简短解析器适用于示例的 JSON tick/done。通用封装还支持 `out.Send(sse.Event{ID, Name, Data, Retry})`；如果应用扩展了数据格式，需要同时定义客户端的解析和错误约定。

## 6. 浏览器 WebSocket 先取 ticket

原生 `new WebSocket()` 同样不能设置 Authorization header。示例先用 Bearer 调用 POST tickets，再用 ticket 完成一次握手：

```javascript
async function connectEcho(token) {
  const response = await fetch('/api/v1/examples/realtime/tickets', {
    method: 'POST',
    headers: { Authorization: `Bearer ${token}` },
  });
  if (!response.ok) throw new Error(`获取连接凭据失败：${response.status}`);
  const { data } = await response.json();
  const url = new URL('/api/v1/examples/realtime/ws', window.location.href);
  url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
  url.searchParams.set('ticket', data.ticket);
  const socket = new WebSocket(url);
  socket.addEventListener('open', () => {
    socket.send(JSON.stringify({ hello: 'WebSocket' }));
  });
  socket.addEventListener('message', event => console.log('echo', event.data));
  socket.addEventListener('error', () => console.error('实时连接失败'));
  socket.addEventListener('close', event => console.log('连接结束', event.code));
  return socket;
}

// const socket = await connectEcho(token);
// 离开页面时：socket.close(1000, 'page closed');
```

ticket 为随机 32 字节，30 秒有效、原子单次消费，最多保留 128 条；发行时惰性清理过期项，无后台清扫 goroutine。它只存在当前进程，重启即失效；普通会话已 logout、过期或失去权限时，尚未使用的 ticket 也无法完成握手。

每次重连都重新取 ticket，失败的握手也可能已经消费 ticket，不要复用旧 URL。不要打印 socket URL、ticket 响应或整个错误事件。框架仅记录注册路由，反向代理也要避免把带查询参数的完整请求写日志。

同源 Origin 自动接受；跨源必须精确配置到 `security.cors_origins`，不使用通配符。HTTPS 在反向代理终止时，后端不会自动信任 `X-Forwarded-*` 来扩大来源范围，应将实际公网 HTTPS origin 显式登记。Origin 校验不替代会话和权限校验。

## 7. Nginx 代理

下面放在 Nginx `http` 配置中，替换域名、证书及服务地址后与现有站点合并。TLS 配置按站点已有配置维护；`location` 示例仅处理实时前缀，登录等普通接口沿用原代理。

```nginx
map $http_upgrade $realtime_connection {
    default upgrade;
    ''      close;
}

# 不用 $request/$request_uri，它们可能包含单次 ticket 查询参数
log_format realtime_safe '$request_method $uri $status $request_time';

server {
    listen 80;
    server_name example.test;

    location /api/v1/examples/realtime/ {
        proxy_pass http://127.0.0.1:18082;
        proxy_http_version 1.1;
        proxy_set_header Host $http_host;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $realtime_connection;

        proxy_buffering off;
        proxy_cache off;
        gzip off;
        proxy_read_timeout 75s;
        proxy_send_timeout 15s;
        access_log /var/log/nginx/realtime_access.log realtime_safe;
    }
}
```

SSE 必须关闭代理缓冲，否则服务端已 flush、浏览器仍会等一批数据。WS 需要 HTTP/1.1 Upgrade。75 秒的空闲读取预算大于默认心跳周期；负载均衡或 CDN 的空闲超时也需要与心跳配合。

## 8. 断线、重连和停机

- 示例 SSE 在 done 后主动结束；WS 在客户端关闭、取消、授权失效、超时或服务停机时关闭。
- 一条连接最长默认五分钟。真实应用可选择带退避和随机抖动的重连，并在 401 时重新登录；避免无间隔重试压满连接或 ticket 容量。
- 此封装不持久化事件、不处理 Last-Event-ID 自动重放，也不保证断线期间消息送达。设置 SSE Event.ID 不等于已经实现事件历史；需要补发时由业务定义游标、存储和幂等规则。
- 停机先拒绝新流、取消现有会话，再在有限预算内等待回调退出；WS 升级连接不依赖 `http.Server.Shutdown` 单独追踪。业务不能启动脱离 ctx 的无限后台任务。
- 协议开始前可返回统一 HTTP 错误；首帧或升级之后只结束协议，绝不追加 `response.JSON`。客户端把异常断开作为连接状态处理。

## 9. 修改之后验证什么

```powershell
go generate ./pkg/wire
go tool wire diff ./pkg/wire
go test ./internal/stream/... ./pkg/serve/controller ./pkg/router/routes -count=1
go vet ./...
go build -o .build/healgo-example ./cmd/example
```

集成测试使用 `HEAGO_TEST_DATABASE_URL`（PostgreSQL）或 `HEAGO_TEST_MYSQL_DSN`（MySQL），两个驱动分别运行；设置方法见[开发手册](DEVELOPMENT.md#5-修改之后怎么验证)。测试使用真实 HTTP 和随机 schema/database，验证 ticket 单次/到期/容量/并发、401/403、浏览器来源、WS echo、底层会话注销及 SSE 前两帧跨普通请求超时。测试自己的 ticket 和 bearer 也不得进入访问日志。

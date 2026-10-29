# 长连接生命周期

## 职责

管理每个应用实例的 WebSocket/SSE 连接数、独立最长时长、取消与停机等待。普通 HTTP 超时继续有效，只有显式使用流封装的处理器取回真实请求取消信号。

## 内容

- [manager.go](manager.go)：`Manager` 与 `Session`；框架默认共享 128 个活动流名额，所有协议合并计数。
- [http.go](http.go)：请求 context 衔接、严格 origin 检查、流开始前的统一错误响应。
- [websocket](websocket/README.md)：双向消息、心跳、写入期限与连接回收。
- [sse](sse/README.md)：事件编码、立即 flush、心跳与慢客户端处理。
- [使用指南](../../docs/STREAMING.md)：独立示例、鉴权、浏览器客户端和代理配置。

## 约定

由 Container 创建共享 Manager，再通过构造函数传给需要它的控制器，不为每个请求另建登记表。`Begin` 在协议开始前执行，返回的 session 必须在处理器真正结束时 `defer End()`；取消不等于已经回收完毕。达到容量上限拒绝新流，暂不建立无界排队或消息历史。

默认流最长 5 分钟，每次写入有单独期限；协议 options 可按需求调整。业务 I/O 必须使用回调 ctx。框架停机先 `Close` 取消全部流，再等待 HTTP 与已升级的 WebSocket 回调结束，最后释放数据库和认证资源。

来源默认允许同源及没有 Origin 的非浏览器客户端；跨域只接受完整 http(s) origin 白名单。Origin 不证明身份，认证/权限仍由路由与应用负责。公共 HTTP timeout 可被流封装绕过，客户端断开、流自身到期、鉴权复核失败和应用停机仍会取消工作。

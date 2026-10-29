# 开发文档

## 职责

解释固定 MVC 目录中一条请求如何经过 routes、controller、service/impl、mapper、model，再返回 VO；说明启动装配、流式通信与验证边界。

## 内容

1. [LAYER_EXAMPLES.md](LAYER_EXAMPLES.md)：先看真实代码与分层写法。
2. [DEVELOPMENT.md](DEVELOPMENT.md)：启动示例、登录、CRUD、版本冲突与源码断点。
3. [CONVENTIONS.md](CONVENTIONS.md)：固定目录、命名、依赖、错误、事务和完成标准。
4. [EXTENDING.md](EXTENDING.md)：新增一个资源、手写/可选 Wire 注入、模型及路由登记、事务与配置接入。
5. [依赖注入说明](../pkg/wire/README.md)：Container、集中 provider sets、生成与资源清理。
6. [API.md](API.md)：系统 API、统一响应、认证、角色权限和持久化配置。
7. [STREAMING.md](STREAMING.md)：WebSocket/SSE、浏览器 ticket、心跳、代理和停机。
8. [DATABASE.md](DATABASE.md)：PostgreSQL/MySQL、主副本配置、显式读取、事务与故障行为。
9. [VERIFICATION.md](VERIFICATION.md)：实际验证与边界；[项目说明](../README.md)提供首次配置和部署入口。

## 约定

文档中的文件、命令和构造函数以实际源码为准；接口变化同步说明。示例只由独立入口开启，通用入口不登记示例模型/路由。文档不保存真实凭据，不把测试 skip 记为通过；新增页面加入索引并检查相对链接。

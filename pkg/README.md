## 职责

按固定 MVC 目录组织应用装配、运行、路由、控制器、业务用例与数据访问，便于从请求一路找到 SQL。

## 内容

- [wire](wire/README.md)：Container、默认手写注入、集中 provider sets 与可选 Wire。
- [bootstrap](bootstrap/README.md)：模型准备、种子、HTTP 生命周期和共享资源关闭。
- [router](router/README.md)：总路由与具体 routes。
- [serve](serve/README.md)：controller、请求 DTO、service/impl、mapper。
- [vo](vo/README.md)：公开响应字段与模型转换。

## 约定

CLI 构造 Container 后调用 router，再将 Config、Resources、Router 和 container.Close 交给 Runtime。wire 不 import router；业务依赖通过构造函数传递，不从全局容器中查找。相关测试运行 `go test ./pkg/...`；真实数据库测试使用独立 schema。

## 职责

按固定职责存放 HTTP 处理、业务用例契约与实现、数据库访问；同一资源跨层使用一致名称，避免把所有逻辑放到 controller。

## 内容

- [controller](controller/README.md)：system、note、realtime 控制器及 [请求 DTO](controller/dto/README.md)。
- [service](service/README.md)：NoteService、NoteMapper 和普通 Go 输入/结果。
- [service/impl](service/impl/README.md)：NoteServiceImpl 的业务规则。
- [mapper](mapper/README.md)：NoteMapper 的 GORM 查询、owner 隔离、CAS 与错误脱敏。
- 响应 VO 位于 [pkg/vo](../vo/README.md)，表模型位于 [internal/model](../../internal/model/README.md)。

## 约定

controller 只依赖服务接口；service/impl 不导入 Gin 或 GORM；mapper 不决定 HTTP 状态。构造函数只绑定依赖，不查表、不迁移、不启动无退出机制的任务。完整规范和代码见 [LAYER_EXAMPLES](../../docs/LAYER_EXAMPLES.md)。

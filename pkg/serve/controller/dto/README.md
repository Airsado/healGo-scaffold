# 请求 DTO

## 职责

定义 HTTP 请求允许提交的字段与形状，不承担业务校验或公开响应。DTO 与模型分开，防止 owner、ID、revision 等内部字段被批量赋值。

## 内容

[note_dto.go](note_dto.go) 定义 CreateNoteRequest 和 UpdateNoteRequest。创建只允许 slug、title、content；修改不允许改变 slug 或 owner，且必须提供 title、content、revision。完整字段标签以源码为准。

PUT 的字段指针用于表达“字段没有提交”，例如真实结构中的：

```go
Title    *string `json:"title" binding:"required"`
Content  *string `json:"content" binding:"required"`
Revision uint64  `json:"revision" binding:"required,gt=0"`
```

这是结构体字段摘录，不是独立 Go 文件。空字符串 content 的指针非 nil，可以主动清空；缺省/null 不能被误当成清空。

## 约定

新请求类型按资源命名，如 CreateNoteRequest；JSON 标签和 BindJSON 负责格式，service/impl 仍独立检查业务规则。响应使用 [pkg/vo](../../../vo/README.md)，不能直接复用 DTO 或 model。controller 解引用前先完成绑定/必填校验。

完整 HTTP 回归在 `go test ./pkg/router/routes -count=1`，需专用 PostgreSQL 或 MySQL 测试连接；相关输入与返回规范见 [分层示例](../../../../docs/LAYER_EXAMPLES.md)。

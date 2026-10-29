# 公开响应 VO

## 职责

明确 API 可以公开的字段，并从持久化模型显式转换。VO 不执行 SQL、不接受可写请求；即使表增加内部列，也不会自动扩大响应范围。

## 内容

[note_vo.go](note_vo.go) 定义 NoteVO、NotePageVO 和 FromNoteModel。ID 的 JSON 标签使用 `,string`，避免浏览器舍入 64 位主键；revision 保持数字，时间由服务端生成。owner 不出现在公开 VO 中。

实际转换函数：

```go
func FromNoteModel(note model.Note) NoteVO {
    return NoteVO{ID: note.ID, Slug: note.Slug, Title: note.Title, Content: note.Content,
        Revision: note.Revision, CreatedAt: note.CreatedAt, UpdatedAt: note.UpdatedAt}
}
```

这是源码摘录，imports 和类型见完整文件。controller 逐条转换分页结果，并分配空 Items，确保没有数据时仍为 `[]`。

## 约定

响应按资源命名 `*VO`，不嵌入 model，不返回密码哈希、密文、token 元数据或内部归属。新增响应字段时先确定公开契约，再显式转换。请求字段写入 [DTO](../serve/controller/dto/README.md)，不复用 VO 作为数据库模型。

相关 JSON/归属边界由 `go test ./pkg/router/routes -count=1` 覆盖，HTTP 集成需专用 PostgreSQL 或 MySQL 测试连接；完整调用链见 [分层示例](../../docs/LAYER_EXAMPLES.md)。

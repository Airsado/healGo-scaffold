## 职责

定义可选笔记示例的持久化结构与跨层稳定错误。模型在 mapper 和 service 之间传递；HTTP 控制器必须显式转为公开 VO，不能直接序列化本结构。

## 内容

| 文件 | 阅读重点 |
|---|---|
| [note.go](note.go) | `Note`、固定表名 `example_notes`、owner 联合索引、revision 和错误类别 |
| [note_test.go](note_test.go) | `json:"-"` 防止模型被误当作公开响应的兜底 |
| [集中登记入口](../get_all_models.go) | 示例开关开启时才将 `&note.Note{}` 加入初始化列表 |
| [服务契约](../../../pkg/serve/service/note_service.go) | 服务输入和存储接口如何使用模型 |
| [GORM mapper](../../../pkg/serve/mapper/note_mapper.go) | owner 查询范围、版本条件更新与安全错误分类 |

以下字段摘自实际源码；完整结构以 [note.go](note.go) 为准：

```go
// 同一个 owner 的 slug 唯一，不同 owner 可以使用同一个 slug
OwnerID uint64 `gorm:"not null;uniqueIndex:uq_example_notes_owner_slug,priority:1;index:idx_example_notes_owner_id,priority:1" json:"-"`
Slug    string `gorm:"size:64;not null;uniqueIndex:uq_example_notes_owner_slug,priority:2" json:"-"`
// revision 用于条件更新，不能用时间戳替代并发版本
Revision uint64 `gorm:"not null;default:1" json:"-"`
```

`idx_example_notes_owner_id` 支持按 owner 过滤并按 ID 倒序列表；`uq_example_notes_owner_slug` 决定并发创建同名资源时只能有一个成功。`Revision` 从 1 开始，更新在同一条 SQL 中比较旧值并加一。

## 约定

这里只定义存储结构，不查询、不迁移、不检查标题和正文格式。业务输入规则在 service 中统一执行；数据库约束提供并发下的最终保证。迁移集中交给 [internal/db](../../db/README.md)，变更已有表时另行评估数据兼容性。

稳定错误不携带 SQL、连接地址、标题或正文。`ErrInvalidInput` 表示用例规则不满足；`ErrNotFound` 表示当前身份范围内不可见；`ErrConflict` 表示唯一键或版本冲突；`ErrUnavailable` 表示存储不可用。上层通过 `errors.Is` 判断，不解析文本。

`json:"-"` 是误序列化时的额外兜底，不能代替控制器的字段选择。即使模型新增内部字段，也必须单独决定是否加入公开 VO。

在模板根目录运行 `go test ./internal/model/note -count=1`，无需 PostgreSQL。

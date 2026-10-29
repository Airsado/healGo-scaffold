## 职责

实现 [service](../README.md) 声明的业务用例：先规范化和检查规则，再调用注入的存储接口。本目录不接触 Gin、GORM、HTTP 状态码或数据库连接，因此相同用例可以由多个入口复用。

## 内容

| 文件 | 阅读重点 |
|---|---|
| [note_service_impl.go](note_service_impl.go) | `NoteServiceImpl`、`NewNoteService`、CRUD 规则、身份与 context 传播 |
| [note_service_impl_test.go](note_service_impl_test.go) | 内存 mapper 替身、规则边界与调用次数断言 |
| [契约和输入](../note_service.go) | 服务接口、存储接口、输入和分页结果 |
| [持久化模型](../../../../internal/model/note/note.go) | 服务构造和接收的内部模型 |
| [GORM 实现](../../mapper/note_mapper.go) | 最终执行 SQL 的 mapper |

下面摘自真实实现，只展示构造与创建用例，省略 imports、辅助函数及其他四个方法。`NewNoteService` 返回完整接口，因此这个片段**不是独立完整文件**，复制业务时应以[完整源码](note_service_impl.go)为起点：

```go
type NoteServiceImpl struct { mapper service.NoteMapper }

// 只保存依赖，不查询、不迁移、不启动后台任务
func NewNoteService(mapper service.NoteMapper) service.NoteService {
    return &NoteServiceImpl{mapper: mapper}
}

func (s *NoteServiceImpl) Create(ctx context.Context, ownerID uint64, input service.CreateNoteInput) (model.Note, error) {
    // 先规范化再判断，避免校验的标题与最终存储结果不同
    input.Title = strings.TrimSpace(input.Title)
    // slug 不悄悄改写，防止两个不同输入被合并成相同唯一地址
    if !validID(ownerID) || len(input.Slug) < 1 || len(input.Slug) > 64 || !slugPattern.MatchString(input.Slug) || !validText(input.Title, input.Content) {
        return model.Note{}, service.ErrInvalidInput
    }
    if s == nil || s.mapper == nil {
        return model.Note{}, service.ErrUnavailable
    }
    // ctx 和 owner 原样进入存储；初始版本固定为 1
    return s.mapper.Create(ctx, model.Note{OwnerID: ownerID, Slug: input.Slug, Title: input.Title, Content: input.Content, Revision: 1})
}
```

规则在入口统一执行，非法输入不会触发 mapper。slug 为 1–64 字节，仅小写 ASCII 字母、数字与分隔单词的单连字符；标题去首尾空白后为 1–120 个 Unicode 字符；正文最多 10000 个字符且允许空字符串。文本必须是有效 UTF-8，不能含 PostgreSQL text 不支持的 NUL。

ID 为正数且不超出两个驱动共同约定的有符号 bigint 正数范围。分页为 1–10000 页、每页 1–100 条。更新和删除需要旧 revision，更新还需预留加一的空间；slug 不在更新输入中。

## 约定

不要为了 HTTP 再增加一套业务校验；HTTP 层只负责协议解析。本层也不能因为相信 controller 已检查，就省去命令行和任务同样需要遵守的规则。方法继续显式接收 owner 与普通 context，不持有请求对象或全局数据库。

更新只把旧 revision 传给 mapper。不能先读取版本、在内存比较后再无条件更新，因为两个并发请求可能同时读到同一个旧值。最终比较与加一由一条 SQL 完成；出现冲突后重新读取并交给调用者处理，不自动覆盖。

用例失败返回稳定类别，由入口映射响应；内部存储已分类的错误直接传递。多个写入需要一起提交时，让应用用例明确事务边界，重新用同一个 tx 构造所需 mapper 与 service，参见 [事务示例](../../../../docs/EXTENDING.md)。构造器自身保持无 I/O。

添加用例时，先声明输入和实际需要的 mapper 方法，再实现规则、更新替身和测试。已有两个测试分别覆盖创建规则与身份/版本/分页边界：

```sh
go test ./pkg/serve/service/impl -run '^TestCreateValidationPreventsPersistenceAndNormalizesTitle$' -count=1
go test ./pkg/serve/service/impl -run '^TestRevisionPaginationAndIdentityBoundaries$' -count=1
```

这些测试不需要数据库；它们检查非法输入不调用存储，以及合法输入如何传递 ctx、owner、规范化后的标题和初始版本。它们不验证真实 SQL 和数据库索引。List 允许复制延迟，详情和写入结果要求主库可见；节点规则由 Mapper 和 [数据库基础设施](../../../../docs/DATABASE.md)落实。

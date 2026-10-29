## 职责

集中保存持久化模型，并在一个明确入口登记需要初始化的表。这里对应 gin-scaffold 的 `internal/model`：模型按业务子包区分，业务处理仍放在 `pkg/serve` 的固定层级中。

## 内容

| 文件或目录 | 用途 |
|---|---|
| [get_all_models.go](get_all_models.go) | `GetAllModels(enableExamples)` 汇总 GORM 模型，不执行 SQL |
| [note/](note/README.md) | 可选 CRUD 教学示例的模型、索引、版本字段和稳定错误 |
| [数据库初始化](../db/README.md) | 连接和集中初始化由基础设施完成，模型目录不自行迁移 |

`GetAllModels` 始终登记认证和持久化设置的基础表；只有显式启用示例时才登记 `note.Note`。普通启动不会因为复制模板就新增示例业务表。

## 约定

模型描述表结构、列与索引，不处理 HTTP、登录、数据库连接或业务流程。新增模型时，在独立子包定义结构，再到 `GetAllModels` 显式登记；不要用 `init()` 或构造器偷偷执行 `AutoMigrate`。

GORM 标签约束的是持久化结构。例如 `not null` 只禁止 SQL NULL，并不保证标题非空；标题规则仍由 service 执行。需要并发保证的唯一性则必须有数据库索引，不能只靠写入前查询。

模型与公开协议分开：请求 DTO 只列允许输入的字段，响应 VO 只列允许公开的字段。不要把 GORM 模型直接绑定 JSON 或返回给客户端。

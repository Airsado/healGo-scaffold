// Package service 定义用例契约、输入与结果，不执行 HTTP 或数据库操作
// 接口和实现分开后，controller 只依赖契约，组合根负责选择具体实现
package service

import (
	"context"

	model "heago-scaffold/internal/model/note"
)

// sentinel 与模型使用同一值，调用方通过 errors.Is 判断，不解析错误文本
// 共享错误放在模型层，mapper 不必反向依赖服务即可返回相同安全类别
var (
	ErrInvalidInput = model.ErrInvalidInput
	ErrNotFound     = model.ErrNotFound
	ErrConflict     = model.ErrConflict
	ErrUnavailable  = model.ErrUnavailable
)

// NoteService 是 controller、命令行或任务共同使用的笔记用例契约
// ownerID 由可信调用方提供；context 贯穿用例和持久化，不能换成后台 context
type NoteService interface {
	Create(context.Context, uint64, CreateNoteInput) (model.Note, error)
	Get(context.Context, uint64, uint64) (model.Note, error)
	List(context.Context, uint64, int, int) (NotePage, error)
	Update(context.Context, uint64, uint64, UpdateNoteInput) (model.Note, error)
	Delete(context.Context, uint64, uint64, uint64) error
}

// NoteMapper 由使用方 service 声明，仅列出当前 CRUD 用例需要的存储操作
// 实现由 mapper 包提供，测试可提供内存替身；服务契约不需要依赖 GORM
type NoteMapper interface {
	Create(context.Context, model.Note) (model.Note, error)
	Get(context.Context, uint64, uint64) (model.Note, error)
	List(context.Context, uint64, int, int) ([]model.Note, int64, error)
	Update(context.Context, uint64, uint64, uint64, string, string) (model.Note, error)
	Delete(context.Context, uint64, uint64, uint64) error
}

// CreateNoteInput 不带 JSON 标签或 HTTP 校验规则，不允许输入覆盖认证身份
// 字段名称带 Note 前缀，多个业务共用 service 包时不会产生通用名称冲突
type CreateNoteInput struct {
	Slug, Title, Content string
}

// UpdateNoteInput 不包含 slug，避免隐藏资源重命名与唯一性变化的语义
// Revision 是调用者读取的旧版本，不是让调用者指定最终写入的新版本
type UpdateNoteInput struct {
	Title, Content string
	Revision       uint64
}

// NotePage 是服务结果，controller 仍须逐条转换成公开 DTO
// Items 在空页也为非 nil 切片，避免调用方维护 null 和空数组两套处理逻辑
type NotePage struct {
	Items          []model.Note
	Page, PageSize int
	Total          int64
}

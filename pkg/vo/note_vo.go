// Package vo 定义 API 响应白名单，不把数据库实体直接暴露给调用方
package vo

import (
	"time"

	model "healgo-scaffold/internal/model/note"
)

// NoteVO 保留现有 JSON 契约；ID 使用字符串，防止 JavaScript 舍入 64 位整数
// owner 仅用于内部查询隔离，不属于公开响应或可写请求字段
type NoteVO struct {
	ID        uint64    `json:"id,string"`
	Slug      string    `json:"slug"`
	Title     string    `json:"title"`
	Content   string    `json:"content"`
	Revision  uint64    `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// NotePageVO 把分页元数据与公开条目放在一起，不复用持久化结构
// 控制器始终分配 Items，使空页仍编码为 [] 而不是 null
type NotePageVO struct {
	Items    []NoteVO `json:"items"`
	Page     int      `json:"page"`
	PageSize int      `json:"pageSize"`
	Total    int64    `json:"total"`
}

// FromNoteModel 显式列出可公开字段，新增数据库列不会自动扩大 API 范围
// 不嵌入 model.Note，避免未来的内部字段跟随匿名序列化泄露
func FromNoteModel(note model.Note) NoteVO {
	return NoteVO{ID: note.ID, Slug: note.Slug, Title: note.Title, Content: note.Content,
		Revision: note.Revision, CreatedAt: note.CreatedAt, UpdatedAt: note.UpdatedAt}
}

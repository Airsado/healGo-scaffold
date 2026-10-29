// Package dto 定义 HTTP 请求白名单；响应对象统一放在 pkg/vo
// 请求不能选择 owner、ID 或初始版本，身份始终来自认证中间件
package dto

// CreateNoteRequest 只表达协议中的可编辑字段
// binding 做必填检查；格式、长度与唯一性规则仍由 service 和数据库保证
type CreateNoteRequest struct {
	Slug    string `json:"slug" binding:"required"`
	Title   string `json:"title" binding:"required"`
	Content string `json:"content"`
}

// UpdateNoteRequest 使用指针区分字段缺省与主动清空正文
// PUT 完整替换 title/content，revision 防止旧页面覆盖新版本
type UpdateNoteRequest struct {
	Title    *string `json:"title" binding:"required"`
	Content  *string `json:"content" binding:"required"`
	Revision uint64  `json:"revision" binding:"required,gt=0"`
}

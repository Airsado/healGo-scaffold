// Package note 定义笔记的持久化模型与跨层错误，不依赖 HTTP 或服务实现
// 共享错误放在底层，避免 mapper 为引用错误而反向依赖 service
package note

import (
	"errors"
	"time"
)

var (
	ErrInvalidInput = errors.New("invalid note input")
	ErrNotFound     = errors.New("note not found")
	ErrConflict     = errors.New("note conflict")
	ErrUnavailable  = errors.New("note storage unavailable")
)

// Note 只在持久化层与服务层之间流动；控制器必须显式转为 DTO
// json 排除是误序列化时的兜底，不能代替控制器选择公开字段
// Slug 的唯一范围是 owner，两个不同账号可以使用同一个地址名
type Note struct {
	ID      uint64 `gorm:"primaryKey;index:idx_example_notes_owner_id,priority:2,sort:desc" json:"-"`
	OwnerID uint64 `gorm:"not null;uniqueIndex:uq_example_notes_owner_slug,priority:1;index:idx_example_notes_owner_id,priority:1" json:"-"`
	Slug    string `gorm:"size:64;not null;uniqueIndex:uq_example_notes_owner_slug,priority:2" json:"-"`
	Title   string `gorm:"size:120;not null" json:"-"`
	Content string `gorm:"type:text;not null" json:"-"`
	// Revision 是并发写入版本；时间戳不足以可靠地区分同一时刻的多个修改
	Revision  uint64    `gorm:"not null;default:1" json:"-"`
	CreatedAt time.Time `gorm:"not null" json:"-"`
	UpdatedAt time.Time `gorm:"not null" json:"-"`
}

// 固定示例表名前缀，避免模板中的可选模块污染框架 sys_* 表语义
func (Note) TableName() string { return "example_notes" }

// Package impl 实现 service 包声明的业务用例，不依赖 Gin 或 GORM
// 实现只依赖窄存储接口；HTTP 与数据库细节在调用链的两端分别适配
package impl

import (
	"context"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"

	model "healgo-scaffold/internal/model/note"
	"healgo-scaffold/pkg/serve/service"
)

// NoteServiceImpl 保存本次装配的存储依赖，不查找全局 DB 或 HTTP 请求对象
// owner、context 和 revision 都由方法入参显式传播，测试也能提供内存替身
type NoteServiceImpl struct {
	mapper service.NoteMapper
}

// NewNoteService 只绑定依赖，不查表、不迁移、不启动后台任务
// 返回 service.NoteService 接口，使 controller 不必依赖具体实现类型
func NewNoteService(mapper service.NoteMapper) service.NoteService {
	return &NoteServiceImpl{mapper: mapper}
}

var slugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func (s *NoteServiceImpl) Create(ctx context.Context, ownerID uint64, input service.CreateNoteInput) (model.Note, error) {
	// 标题允许正常排版空白，但持久化前规范首尾空白，让长度规则和展示结果一致
	input.Title = strings.TrimSpace(input.Title)
	// Slug 不自动改写：悄悄转小写或删字符会把用户输入合并成同一个唯一地址
	if !validID(ownerID) || len(input.Slug) < 1 || len(input.Slug) > 64 || !slugPattern.MatchString(input.Slug) || !validText(input.Title, input.Content) {
		return model.Note{}, service.ErrInvalidInput
	}
	if s == nil || s.mapper == nil {
		return model.Note{}, service.ErrUnavailable
	}
	return s.mapper.Create(ctx, model.Note{OwnerID: ownerID, Slug: input.Slug, Title: input.Title, Content: input.Content, Revision: 1})
}

func (s *NoteServiceImpl) Get(ctx context.Context, ownerID, id uint64) (model.Note, error) {
	// 服务入口先拒绝无法作为持久化主键的值，避免把参数错误变成驱动故障
	if !validID(ownerID) || !validID(id) {
		return model.Note{}, service.ErrInvalidInput
	}
	if s == nil || s.mapper == nil {
		return model.Note{}, service.ErrUnavailable
	}
	// 不自行先读全量数据再检查所有权；mapper 必须把身份范围直接放进查询
	return s.mapper.Get(ctx, ownerID, id)
}

func (s *NoteServiceImpl) List(ctx context.Context, ownerID uint64, page, pageSize int) (service.NotePage, error) {
	// 同时限制页码与页长，乘法在 32 位平台也不会溢出，数据库偏移也有明确上限
	if !validID(ownerID) || page < 1 || page > 10000 || pageSize < 1 || pageSize > 100 {
		return service.NotePage{}, service.ErrInvalidInput
	}
	if s == nil || s.mapper == nil {
		return service.NotePage{}, service.ErrUnavailable
	}
	items, total, err := s.mapper.List(ctx, ownerID, (page-1)*pageSize, pageSize)
	if err != nil {
		return service.NotePage{}, err
	}
	// 统一空集合，避免 API 消费者在 null 与 [] 之间维护两套列表逻辑
	if items == nil {
		items = []model.Note{}
	}
	return service.NotePage{Items: items, Page: page, PageSize: pageSize, Total: total}, nil
}

func (s *NoteServiceImpl) Update(ctx context.Context, ownerID, id uint64, input service.UpdateNoteInput) (model.Note, error) {
	input.Title = strings.TrimSpace(input.Title)
	// PostgreSQL bigint 是有符号的；预留下一版空间，避免合法请求在 SQL 加一时溢出
	if !validID(ownerID) || !validID(id) || input.Revision < 1 || input.Revision >= math.MaxInt64 || !validText(input.Title, input.Content) {
		return model.Note{}, service.ErrInvalidInput
	}
	if s == nil || s.mapper == nil {
		return model.Note{}, service.ErrUnavailable
	}
	// 原样传递客户端读取的版本，存储层在一个条件更新中完成比较与自增
	// 不能在这里先 Get 后更新，否则两个请求会同时读到同一版本并互相覆盖
	return s.mapper.Update(ctx, ownerID, id, input.Revision, input.Title, input.Content)
}

func (s *NoteServiceImpl) Delete(ctx context.Context, ownerID, id, revision uint64) error {
	// 删除也使用版本，旧列表页面不能无提示地删掉刚被另一请求修改的内容
	if !validID(ownerID) || !validID(id) || !validID(revision) {
		return service.ErrInvalidInput
	}
	if s == nil || s.mapper == nil {
		return service.ErrUnavailable
	}
	return s.mapper.Delete(ctx, ownerID, id, revision)
}

// 应用使用 uint64 表达非负 ID，PostgreSQL bigint 的实际正数范围仍要明确限制
func validID(id uint64) bool { return id > 0 && id <= math.MaxInt64 }

func validText(title, content string) bool {
	// 标题和正文按字符计数，中文不因 UTF-8 占三个字节而提前达到长度上限
	// PostgreSQL text 不接受 NUL，提前拒绝才能把输入错误稳定映射成 400
	// 正文可以为空，这是一种有效修改，不能被“忽略零值”的存储行为吞掉
	return utf8.ValidString(title) && utf8.ValidString(content) && !strings.ContainsRune(title, 0) && !strings.ContainsRune(content, 0) && utf8.RuneCountInString(title) >= 1 && utf8.RuneCountInString(title) <= 120 && utf8.RuneCountInString(content) <= 10000
}

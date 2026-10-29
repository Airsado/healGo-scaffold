// Package controller translates note HTTP requests into service calls.
// It owns DTOs and status codes; transactions and ownership filters stay below.
//
// HTTP 层解决“请求如何表达”，服务层解决“这次操作是否符合业务规则”
// 因而控制器不执行 SQL，也不先查询再自行决定版本是否冲突
// 每个操作显式传 ownerID，让相同权限下的不同用户仍然隔离
// 请求 DTO 则只列出可写字段，拒绝把数据库模型当作输入对象绑定
//
// 分页和删除参数采用严格解析：默认值仅用于缺省，而不是修复错误输入
// 业务错误使用固定 code，客户端能够区分重新登录、重新读取和稍后重试
// 原始错误可能包含 SQL 或用户正文，不能直接写进响应或业务日志
package controller

import (
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"

	"heago-scaffold/internal/middleware"
	"heago-scaffold/internal/response"
	"heago-scaffold/pkg/serve/controller/dto"
	"heago-scaffold/pkg/serve/service"
	"heago-scaffold/pkg/vo"

	"github.com/gin-gonic/gin"
)

type NoteController struct{ service service.NoteService }

// NewNoteController 只绑定 service 契约，不查数据库，手写注入与 Wire 均可直接使用
func NewNoteController(notes service.NoteService) *NoteController {
	return &NoteController{service: notes}
}

func (h *NoteController) Create(c *gin.Context) {
	// 新建记录也必须有真实 owner，不能因还没有记录 ID 就跳过身份约束
	// slug 的合法性和同用户唯一性由服务与数据库共同保证
	// 控制器只做必填和类型检查，避免复制一份会逐渐分叉的业务规则
	owner, ok := ownerID(c)
	if !ok {
		return
	}
	var input dto.CreateNoteRequest
	// The shared decoder rejects unknown fields, including a forged owner_id.
	if !response.BindJSON(c, &input) {
		return
	}
	note, err := h.service.Create(c.Request.Context(), owner, service.CreateNoteInput{
		Slug: input.Slug, Title: input.Title, Content: input.Content,
	})
	if err != nil {
		fail(c, err)
		return
	}
	response.Write(c, http.StatusCreated, vo.FromNoteModel(note))
}

func (h *NoteController) Get(c *gin.Context) {
	// 对别人的记录统一按不存在处理，不额外泄露其标题或存在状态
	// 权限不足由路由提前返回 403，资源归属判断由下层查询完成
	owner, ok := ownerID(c)
	if !ok {
		return
	}
	id, ok := noteID(c)
	if !ok {
		return
	}
	// Passing owner to every operation makes isolation independent of the route
	// permission. A user with read permission still cannot read another owner.
	note, err := h.service.Get(c.Request.Context(), owner, id)
	if err != nil {
		fail(c, err)
		return
	}
	response.Success(c, vo.FromNoteModel(note))
}

func (h *NoteController) List(c *gin.Context) {
	// List 与单条读取使用相同的 owner 条件，分页 total 也不能统计全站记录
	// ParseQuery 会报告错误转义或分号参数，URL.Query 则会静默丢弃部分参数
	owner, ok := ownerID(c)
	if !ok {
		return
	}
	query, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil || !onlyKeys(query, "page", "pageSize") {
		invalid(c)
		return
	}
	page, okPage := positiveQuery(query, "page", 1, 10_000)
	// 限制页码与每页数量，使 offset 和单次结果集都保持在明确范围内
	// 服务还会独立校验，后台任务直接调用服务时同样遵守这些规则
	size, okSize := positiveQuery(query, "pageSize", 20, 100)
	if !okPage || !okSize {
		invalid(c)
		return
	}
	result, err := h.service.List(c.Request.Context(), owner, int(page), int(size))
	if err != nil {
		fail(c, err)
		return
	}
	// An empty list remains [] rather than null, keeping the client contract stable.
	items := make([]vo.NoteVO, 0, len(result.Items))
	for _, item := range result.Items {
		items = append(items, vo.FromNoteModel(item))
	}
	response.Success(c, vo.NotePageVO{Items: items, Page: result.Page, PageSize: result.PageSize, Total: result.Total})
}

func (h *NoteController) Update(c *gin.Context) {
	// PUT 是完整替换可编辑字段，省略 content 与把 content 清空是不同意图
	// DTO 的指针在通过必填校验后才解引用，不把缺字段当成空字符串保存
	owner, ok := ownerID(c)
	if !ok {
		return
	}
	id, ok := noteID(c)
	if !ok {
		return
	}
	var input dto.UpdateNoteRequest
	if !response.BindJSON(c, &input) {
		return
	}
	// PUT cannot change the slug or owner, and requires both replaceable fields.
	// A revision conflict asks the client to reload instead of overwriting blindly.
	note, err := h.service.Update(c.Request.Context(), owner, id, service.UpdateNoteInput{
		Title: *input.Title, Content: *input.Content, Revision: input.Revision,
	})
	if err != nil {
		fail(c, err)
		return
	}
	response.Success(c, vo.FromNoteModel(note))
}

func (h *NoteController) Delete(c *gin.Context) {
	// 删除带版本参数与更新采用相同的并发约束，避免“刚改好就被旧页删掉”
	// 实际比较和删除必须由下层原子完成，不能在控制器分成两次 SQL
	owner, ok := ownerID(c)
	if !ok {
		return
	}
	id, ok := noteID(c)
	if !ok {
		return
	}
	query, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil {
		invalid(c)
		return
	}
	revision, ok := positiveQuery(query, "revision", 0, math.MaxInt64)
	if !ok || !onlyKeys(query, "revision") {
		invalid(c)
		return
	}
	// Deletion also uses a revision: a stale tab cannot delete a newer edit.
	if err := h.service.Delete(c.Request.Context(), owner, id, revision); err != nil {
		fail(c, err)
		return
	}
	response.Success(c, gin.H{"deleted": true})
}

// The handler remains fail-closed if it is accidentally registered without the
// authentication group; a zero owner ID must never become a shared namespace.
func ownerID(c *gin.Context) (uint64, bool) {
	// 认证中间件已从数据库核实账号状态，这里读取的是服务端上下文
	// 即使请求带 user_id 或 owner_id，也不会成为此处身份的来源
	p, ok := middleware.Principal(c)
	if !ok || p.ID == 0 {
		response.Fail(c, 401, "unauthorized", "请重新登录")
		return 0, false
	}
	return p.ID, true
}

func noteID(c *gin.Context) (uint64, bool) {
	// Go uint64 大于 PostgreSQL bigint 范围的值应作为参数错误拒绝
	// 不让不可表示的 ID 流到 SQL，再被误报为服务不可用
	id, ok := positive(c.Param("id"), math.MaxInt64)
	if !ok {
		invalid(c)
	}
	return id, ok
}

// Reject repeated, empty or misspelled query parameters instead of silently
// picking one interpretation. Defaults apply only when the key is absent.
func positiveQuery(query url.Values, key string, defaultValue, maximum uint64) (uint64, bool) {
	// Values 保留所有同名参数，能够拒绝代理与服务可能解释不同的重复值
	// 空字符串不采用默认值，否则 page= 的拼写错误会悄悄变成第一页
	values, present := query[key]
	if !present {
		return defaultValue, defaultValue > 0
	}
	if len(values) != 1 {
		return 0, false
	}
	return positive(values[0], maximum)
}

func positive(value string, maximum uint64) (uint64, bool) {
	// 只接受一种正整数表示，不接受正号、前导零、小数或科学计数法
	// 路径和 query 共用规则，客户端不必猜同一个 ID 在各接口的解释差异
	if value == "" || value[0] < '1' || value[0] > '9' {
		return 0, false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return 0, false
		}
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	// 逐字符检查不能防止整数溢出，仍必须处理 ParseUint 的错误
	return parsed, err == nil && parsed <= maximum
}

func onlyKeys(query url.Values, allowed ...string) bool {
	// 白名单拒绝拼错的 page_size，也防止调用方误以为 owner_id 生效
	// 这是参数契约检查，真正的数据隔离仍靠认证身份与数据库 owner 条件
	for key := range query {
		found := false
		for _, candidate := range allowed {
			found = found || key == candidate
		}
		if !found {
			return false
		}
	}
	return true
}

func invalid(c *gin.Context) {
	// 提示描述字段类别，不回显原始字符串，避免敏感正文进入错误响应
	response.Fail(c, 400, "example_note_invalid_input", "笔记内容、ID、分页或版本参数不正确")
}

// Never echo a raw SQL error or request value. Stable error codes let clients
// handle a stale revision differently from an absent resource or an outage.
func fail(c *gin.Context, err error) {
	// 只按 errors.Is 分类，不依赖驱动错误文本，也不把 unknown error 当成功
	// 未识别错误按暂时不可用拒绝，保证服务失败不会触发不完整的后续写入
	switch {
	case errors.Is(err, service.ErrInvalidInput):
		invalid(c)
	case errors.Is(err, service.ErrNotFound):
		response.Fail(c, 404, "example_note_not_found", "笔记不存在")
	case errors.Is(err, service.ErrConflict):
		response.Fail(c, 409, "example_note_conflict", "笔记标识已存在或版本已更新，请重新读取")
	default:
		response.Fail(c, 503, "example_note_unavailable", "笔记服务暂不可用")
	}
}

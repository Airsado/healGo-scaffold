// Package controller translates HTTP requests into explicit service calls.
package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"healgo-scaffold/internal/auth"
	"healgo-scaffold/internal/middleware"
	"healgo-scaffold/internal/response"
	"healgo-scaffold/internal/settings"

	"github.com/gin-gonic/gin"
)

// SystemController 只把 HTTP 输入转换成服务调用，不负责拼 SQL 或决定权限继承
// 通过依赖注入组合服务，后续替换存储实现时不必重写接口协议
type SystemController struct {
	Auth           *auth.Service
	Settings       *settings.Service
	SessionSeconds int64
}

// NewSystemController 仅保存已经构造的服务，路由在 pkg/router/routes 显式声明
// 构造阶段不查询数据库，便于手写注入与集中初始化保持同样的启动顺序
func NewSystemController(authentication *auth.Service, configuration *settings.Service, sessionSeconds int64) *SystemController {
	return &SystemController{Auth: authentication, Settings: configuration, SessionSeconds: sessionSeconds}
}

// Me 只返回认证中间件已经确认的身份，不从请求正文读取账号 ID
func (s *SystemController) Me(c *gin.Context) {
	p, _ := middleware.Principal(c)
	response.Success(c, p)
}
func (s *SystemController) Login(c *gin.Context) {
	// 请求 DTO 与数据库账号模型分离，客户端不能提交 enabled、密码哈希等内部字段
	// 长度限制是传输层的第一道防线；账号格式和密码验证仍由服务层负责
	var input struct {
		Username string `json:"username" binding:"required,max=64"`
		Password string `json:"password" binding:"required,max=72"`
	}
	if !response.BindJSON(c, &input) {
		return
	}
	// 传递请求 context，让客户端断开或超时能够取消随后的数据库查询
	// 登录失败使用统一错误，避免通过响应差异判断某个账号是否存在
	token, p, err := s.Auth.Login(c.Request.Context(), input.Username, input.Password)
	if err != nil {
		failAuth(c, err)
		return
	}
	// token 只在成功登录时交付；后续身份由 Authorization 请求头证明
	// expires_in 使用秒，便于客户端提示过期；服务端会话检查才是最终有效性依据
	response.Success(c, gin.H{"token": token, "token_type": "Bearer", "expires_in": s.SessionSeconds, "user": p})
}
func (s *SystemController) Logout(c *gin.Context) {
	// 使用认证中间件已经解析的 token，避免不同处理层对请求头产生不同理解
	// 服务端撤销会话后，浏览器即使仍保留字符串也不能继续访问保护接口
	if err := s.Auth.Logout(middleware.Token(c)); err != nil {
		failAuth(c, err)
		return
	}
	response.Success(c, gin.H{"logged_out": true})
}
func (s *SystemController) Password(c *gin.Context) {
	// 修改密码需要再次证明旧密码，防止短暂取得已登录页面的人直接接管账号
	// bcrypt 有 72 字节上限；这里的标签校验之外，服务层还负责精确的字节检查
	var input struct {
		Old string `json:"old_password" binding:"required,max=72"`
		New string `json:"new_password" binding:"required,max=72"`
	}
	if !response.BindJSON(c, &input) {
		return
	}
	// 目标账号来自当前身份，不接收客户端传入的 user_id，避免越权改他人密码
	p, _ := middleware.Principal(c)
	if err := s.Auth.ChangePassword(c.Request.Context(), p.ID, input.Old, input.New); err != nil {
		failAuth(c, err)
		return
	}
	// Persisted auth version invalidates every existing session, including this one.
	response.Success(c, gin.H{"changed": true, "reauthenticate": true})
}
func (s *SystemController) ListSettings(c *gin.Context) {
	// 配置列表返回服务层定义的公开视图，敏感值不会因为复用数据库模型而泄漏
	// 只读页面也走同一服务入口，保证类型、脱敏与持久化行为保持一致
	items, err := s.Settings.List(c.Request.Context())
	if err != nil {
		failSettings(c, err)
		return
	}
	response.Success(c, gin.H{"items": items})
}
func (s *SystemController) GetSetting(c *gin.Context) {
	// key 只能访问启动时注册的配置项；URL 参数不会创建任意数据库字段
	// “未注册”与“已注册但尚无保存值”由服务层区分，控制器只映射结果
	item, err := s.Settings.Get(c.Request.Context(), c.Param("key"))
	if err != nil {
		failSettings(c, err)
		return
	}
	response.Success(c, item)
}
func (s *SystemController) UpdateSetting(c *gin.Context) {
	// RawMessage 保留 JSON 值的实际类型，由配置注册表决定它应是字符串、数字还是对象
	// revision 是乐观锁版本：客户端必须先读取，再明确基于哪个版本提交修改
	var input struct {
		Value    json.RawMessage `json:"value"`
		Revision uint64          `json:"revision" binding:"required,gt=0"`
	}
	if !response.BindJSON(c, &input) {
		return
	}
	// 缺少 value 与显式提交 null 不同；前者是协议错误，后者交给具体配置规则处理
	if len(input.Value) == 0 {
		response.Fail(c, 400, "invalid_input", "必须提供 value，不能省略")
		return
	}
	p, _ := middleware.Principal(c)
	// The actor comes from the authenticated principal; callers cannot forge audit ownership.
	// 更新值、增加版本和写审计由服务层事务一并完成，控制器不拆成几次独立写入
	item, err := s.Settings.Update(c.Request.Context(), c.Param("key"), input.Value, input.Revision, strconv.FormatUint(p.ID, 10))
	if err != nil {
		failSettings(c, err)
		return
	}
	response.Success(c, item)
}
func (s *SystemController) Audits(c *gin.Context) {
	// 审计记录单独分页，不随配置列表无限增长，也不向客户端返回历史明文密钥
	// 审计读取使用独立权限，普通配置读取权限不能自动获得全部操作历史
	limit, offset, ok := pagination(c)
	if !ok {
		return
	}
	items, total, err := s.Settings.Audit(c.Request.Context(), limit, offset)
	if err != nil {
		failSettings(c, err)
		return
	}
	response.Success(c, gin.H{"items": items, "total": total, "limit": limit, "offset": offset})
}
func (s *SystemController) Users(c *gin.Context) {
	// 管理员列表只暴露账号视图；密码哈希与会话材料不属于列表 DTO
	// 分页同时返回 total，前端可以展示总数而无需一次加载所有账号
	limit, offset, ok := pagination(c)
	if !ok {
		return
	}
	items, total, err := s.Auth.ListUsers(c.Request.Context(), limit, offset)
	if err != nil {
		failAuth(c, err)
		return
	}
	response.Success(c, gin.H{"items": items, "total": total, "limit": limit, "offset": offset})
}
func (s *SystemController) CreateUser(c *gin.Context) {
	// roles 是受控角色名列表，不允许客户端直接声明任意权限字符串
	// dive 对每个元素校验；仅限制数组总长度无法阻止空角色或超长角色名
	var input struct {
		Username string   `json:"username" binding:"required,min=3,max=64"`
		Password string   `json:"password" binding:"required,max=72"`
		Roles    []string `json:"roles" binding:"max=32,dive,required,max=64"`
	}
	if !response.BindJSON(c, &input) {
		return
	}
	// 账号唯一性与角色存在性由服务层和数据库共同保证，不能依赖前端预检查
	p, err := s.Auth.CreateUser(c.Request.Context(), input.Username, input.Password, input.Roles)
	if err != nil {
		failAuth(c, err)
		return
	}
	// 创建资源用 201，客户端可以从 HTTP 状态区分创建成功与普通查询成功
	response.Write(c, http.StatusCreated, p)
}
func (s *SystemController) UserRoles(c *gin.Context) {
	// 路径 ID 先校验再进服务，拒绝负数、溢出和零，而不是让数据库猜测含义
	// 这是替换完整角色集合的操作，空数组表达清空，省略字段的协议语义由 DTO 决定
	id, ok := positiveID(c)
	if !ok {
		return
	}
	var input struct {
		Roles []string `json:"roles" binding:"max=32,dive,required,max=64"`
	}
	if !response.BindJSON(c, &input) {
		return
	}
	// 受保护的系统角色和最后管理员约束属于服务规则，不能只藏在某个页面按钮里
	if err := s.Auth.SetUserRoles(c.Request.Context(), id, input.Roles); err != nil {
		failAuth(c, err)
		return
	}
	response.Success(c, gin.H{"updated": true})
}
func (s *SystemController) UserEnabled(c *gin.Context) {
	// 开关接口与角色接口分开，使每次写操作的意图与错误边界更明确
	id, ok := positiveID(c)
	if !ok {
		return
	}
	// *bool 区分“没有传 enabled”和“明确传 false”；普通 bool 无法表达这个区别
	var input struct {
		Enabled *bool `json:"enabled" binding:"required"`
	}
	if !response.BindJSON(c, &input) {
		return
	}
	// 禁用后旧 token 是否失效由认证服务保证，而不是等待浏览器主动退出
	if err := s.Auth.SetEnabled(c.Request.Context(), id, *input.Enabled); err != nil {
		failAuth(c, err)
		return
	}
	response.Success(c, gin.H{"updated": true})
}
func (s *SystemController) Roles(c *gin.Context) {
	// 角色及权限的真实来源是持久化策略；页面展示结果不能充当授权依据
	// 后续业务模块复用这些系统能力，无需把自己的业务表加入控制器
	items, err := s.Auth.ListRoles(c.Request.Context())
	if err != nil {
		failAuth(c, err)
		return
	}
	response.Success(c, gin.H{"items": items})
}
func (s *SystemController) PutRole(c *gin.Context) {
	// 路径确定角色，正文只包含其权限集合，避免两个位置提供冲突的角色名
	// 权限字符串有总数和单项长度限制，防止管理接口变成无界存储入口
	var input struct {
		Permissions []string `json:"permissions" binding:"max=100,dive,required,max=128"`
	}
	if !response.BindJSON(c, &input) {
		return
	}
	// 服务层验证权限语法与保留角色规则，HTTP 层不复制这些领域判断
	if err := s.Auth.PutRole(c.Request.Context(), c.Param("name"), input.Permissions); err != nil {
		failAuth(c, err)
		return
	}
	response.Success(c, gin.H{"updated": true})
}

// Only stable errors cross the transport boundary; raw SQL or library errors may
// carry secret inputs and are deliberately not used as user-visible messages.
func failAuth(c *gin.Context, err error) {
	// errors.Is 保留包装后的错误分类，让底层增加上下文时不破坏 HTTP 协议
	// 只有白名单错误映射到可预期客户端状态；其他错误统一按服务不可用处理
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials), errors.Is(err, auth.ErrUnauthorized):
		// 401 表示身份凭证不成立，客户端应重新登录，而不是不断重试同一 token
		response.Fail(c, 401, "unauthorized", "账号、密码或登录状态无效")
	case errors.Is(err, auth.ErrForbidden):
		// 403 表示身份已知但操作被禁止；重新登录通常不能改变权限结果
		response.Fail(c, 403, "forbidden", "该操作不被允许")
	case errors.Is(err, auth.ErrInvalidInput):
		// 格式错误由调用者修改请求解决，不伪装成服务器内部故障
		response.Fail(c, 400, "invalid_input", "账号、密码、角色或权限格式不正确")
	case errors.Is(err, auth.ErrNotFound):
		// 仅在已经通过路由权限检查的管理操作中报告目标不存在
		response.Fail(c, 404, "not_found", "账号或角色不存在")
	case errors.Is(err, auth.ErrConflict):
		// 409 描述与当前数据状态的冲突，适合账号重名等无法靠原样重试解决的情形
		response.Fail(c, 409, "conflict", "账号已存在或操作冲突")
	default:
		// 不把 SQL、bcrypt 或会话库的原始错误发送给外部，避免暴露内部实现与输入
		response.Fail(c, 503, "auth_unavailable", "账号服务暂不可用")
	}
}
func failSettings(c *gin.Context, err error) {
	// 配置写入的失败类型决定前端下一步：改值、重新读取，或稍后再试
	// 统一映射放在此处，列表、详情和写入接口不会各自发明一套错误码
	switch {
	case errors.Is(err, settings.ErrNotFound):
		response.Fail(c, 404, "setting_not_found", "配置项未注册")
	case errors.Is(err, settings.ErrInvalidInput):
		response.Fail(c, 400, "invalid_setting", "配置值未通过类型或规则校验")
	case errors.Is(err, settings.ErrConflict):
		// 版本冲突需要重新读取并决定如何合并；自动覆盖会让另一位操作者的修改丢失
		response.Fail(c, 409, "revision_conflict", "配置已被其他请求修改，请重新读取后保存")
	default:
		response.Fail(c, 503, "settings_unavailable", "配置服务暂不可用")
	}
}
func positiveID(c *gin.Context) (uint64, bool) {
	// ParseUint 同时拒绝负数和超过 64 位的数字，不先转成平台相关的 int
	// 零通常代表尚未持久化的模型，不应作为公开管理接口的目标
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		response.Fail(c, 400, "invalid_id", "ID 必须为正整数")
		return 0, false
	}
	return id, true
}
func pagination(c *gin.Context) (int, int, bool) {
	// 统一默认页大小与上限，防止每个列表接口的资源消耗规则逐渐分叉
	// offset 也设上限，因为很深的偏移查询仍需要数据库跳过大量记录
	// 本模板采用易理解的偏移分页；大数据业务应在自己的模块选择游标分页
	limit, err1 := strconv.Atoi(c.DefaultQuery("limit", "20"))
	offset, err2 := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if err1 != nil || err2 != nil || limit < 1 || limit > 100 || offset < 0 || offset > 1000000 {
		response.Fail(c, 400, "invalid_page", "limit 为 1–100，offset 为 0–1000000")
		return 0, 0, false
	}
	return limit, offset, true
}

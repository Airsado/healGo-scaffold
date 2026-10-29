package middleware

import (
	"errors"

	"healgo-scaffold/internal/auth"
	"healgo-scaffold/internal/response"

	"github.com/gin-gonic/gin"
)

// 只有认证中间件写入这两个键；控制器读取的是已验证的请求上下文
// 命名空间前缀避免未来业务模块使用普通字符串键时意外覆盖身份
const principalKey = "heago.principal"
const tokenKey = "heago.token"

// Authenticate verifies an opaque Sa-Token session and reloads the enabled user.
// A cached browser login or user ID supplied in a request is never an identity source.
func Authenticate(service *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 会话有效性和数据库中的启用状态都需要成立，不能只相信 token 尚未到期
		principal, token, err := service.Authenticate(c.Request.Context(), c.Request)
		if err != nil {
			if errors.Is(err, auth.ErrUnavailable) {
				// 后端暂时不可用用 503；若误报 401，客户端会无谓地清空有效登录状态
				response.Fail(c, 503, "auth_unavailable", "认证服务暂不可用")
			} else {
				// 对外统一拒绝非法或失效凭证，不透露具体会话是否曾经存在
				response.Fail(c, 401, "unauthorized", "请重新登录")
			}
			return
		}
		// Principal 是服务层返回的身份快照，不是从请求正文反序列化的用户对象
		c.Set(principalKey, principal)
		c.Set(tokenKey, token)
		// 只有认证完成后才执行下一层；失败分支的 response.Fail 会终止整条处理链
		c.Next()
	}
}

// Permission and Role are separate hooks so a module declares authorization at
// its route, while the service remains responsible for the persisted policy.
func Permission(service *auth.Service, permission string) gin.HandlerFunc {
	// permission 由模块注册路由时声明，客户端不能通过参数指定“要检查的权限”
	return func(c *gin.Context) {
		p, ok := Principal(c)
		if !ok {
			// 即使模块忘了先挂认证中间件，也默认拒绝访问，而不尝试推断身份
			response.Fail(c, 401, "unauthorized", "请重新登录")
			return
		}
		// 授权检查重新核对持久化策略，让角色调整与禁用操作对后续请求及时生效
		err := service.RequirePermission(c.Request.Context(), p, permission)
		// A password change between authentication and authorization invalidates
		// this identity, rather than merely denying one permission.
		if errors.Is(err, auth.ErrUnauthorized) {
			response.Fail(c, 401, "unauthorized", "请重新登录")
			return
		}
		if errors.Is(err, auth.ErrUnavailable) {
			response.Fail(c, 503, "auth_unavailable", "权限服务暂不可用")
			return
		}
		if err != nil {
			// 身份仍成立但缺少权限时返回 403，区别于要求重新登录的 401
			response.Fail(c, 403, "forbidden", "没有此操作的权限")
			return
		}
		c.Next()
	}
}

// Role 适合明确依赖身份分组的入口；多数具体操作更适合声明 Permission
// 两种钩子都使用同一认证来源，不需要业务模块自行解析 token
func Role(service *auth.Service, role string) gin.HandlerFunc {
	return func(c *gin.Context) {
		p, ok := Principal(c)
		if !ok {
			response.Fail(c, 401, "unauthorized", "请重新登录")
			return
		}
		// 服务层负责保留角色和会话版本规则，中间件仅把结果映射成 HTTP 行为
		err := service.RequireRole(c.Request.Context(), p, role)
		if errors.Is(err, auth.ErrUnauthorized) {
			response.Fail(c, 401, "unauthorized", "请重新登录")
			return
		}
		if errors.Is(err, auth.ErrUnavailable) {
			response.Fail(c, 503, "auth_unavailable", "权限服务暂不可用")
			return
		}
		if err != nil {
			response.Fail(c, 403, "forbidden", "没有此角色的权限")
			return
		}
		c.Next()
	}
}

// 带 bool 的访问器迫使调用者处理“尚未认证”的情形，避免把零值 ID 当作真实用户
// 类型断言也防止错误的模块写入同名键后，控制器误把任意对象当作身份使用
func Principal(c *gin.Context) (auth.Principal, bool) {
	value, ok := c.Get(principalKey)
	p, valid := value.(auth.Principal)
	return p, ok && valid
}

// Token 仅供退出等会话操作使用；业务日志与普通 DTO 不应包含此值
func Token(c *gin.Context) string { return c.GetString(tokenKey) }

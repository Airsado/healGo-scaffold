// Package routes 集中声明 URL、HTTP 方法、权限中间件与控制器方法
// 构造依赖与初始化模型发生在接线入口，路由文件不执行数据库操作
package routes

import (
	"healgo-scaffold/internal/middleware"
	"healgo-scaffold/pkg/wire"

	"github.com/gin-gonic/gin"
)

// RegisterSystemRoutes 从已构造的容器取系统控制器，公开与保护组由中央入口提供
// 登录限流闭包只在登记时创建一次，在 bcrypt 校验之前限制尝试次数
func RegisterSystemRoutes(container *wire.Container, public, protected *gin.RouterGroup) {
	system := container.SystemController
	loginBudget := middleware.LoginBudget(container.Config.Auth.LoginAttemptsPerMinute)
	public.POST("/auth/login", loginBudget, system.Login)
	protected.GET("/auth/me", system.Me)
	protected.POST("/auth/logout", system.Logout)
	protected.PUT("/auth/password", system.Password)
	// 登录保护只证明身份，系统管理操作仍分别声明权限
	// 权限服务由容器注入，路由不读取请求字段来决定需要检查的权限
	protected.GET("/system/settings", middleware.Permission(container.Auth, "system:settings:read"), system.ListSettings)
	protected.GET("/system/settings/:key", middleware.Permission(container.Auth, "system:settings:read"), system.GetSetting)
	protected.PUT("/system/settings/:key", middleware.Permission(container.Auth, "system:settings:write"), system.UpdateSetting)
	protected.GET("/system/setting-audits", middleware.Permission(container.Auth, "system:settings:audit"), system.Audits)
	protected.GET("/system/users", middleware.Permission(container.Auth, "system:users:read"), system.Users)
	protected.POST("/system/users", middleware.Permission(container.Auth, "system:users:write"), system.CreateUser)
	protected.PUT("/system/users/:id/roles", middleware.Permission(container.Auth, "system:users:write"), system.UserRoles)
	protected.PUT("/system/users/:id/enabled", middleware.Permission(container.Auth, "system:users:write"), system.UserEnabled)
	protected.GET("/system/roles", middleware.Permission(container.Auth, "system:roles:read"), system.Roles)
	protected.PUT("/system/roles/:name", middleware.Permission(container.Auth, "system:roles:write"), system.PutRole)
}

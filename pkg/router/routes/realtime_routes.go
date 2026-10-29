package routes

import (
	"heago-scaffold/internal/auth"
	"heago-scaffold/internal/middleware"
	"heago-scaffold/pkg/serve/controller"

	"github.com/gin-gonic/gin"
)

// RegisterRealtimeRoutes 保留原生浏览器 WebSocket 的 ticket 接入方式
// 公开握手路由仍在 controller 内认证；它不能因为缺少 bearer 请求头而跳过权限
func RegisterRealtimeRoutes(public, protected *gin.RouterGroup, realtime *controller.RealtimeController, authentication *auth.Service) {
	public.GET("/examples/realtime/ws", realtime.WebSocket)
	// ticket 签发和 SSE 支持 Authorization，先走普通的受保护路由再复查会话
	// 使用注入的认证服务，不访问控制器私有字段或创建第二套认证状态
	group := protected.Group("/examples/realtime", middleware.Permission(authentication, controller.RealtimePermission))
	group.GET("/sse", realtime.SSE)
	group.POST("/tickets", realtime.Ticket)
}

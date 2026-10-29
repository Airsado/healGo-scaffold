package routes

import (
	"heago-scaffold/internal/auth"
	"heago-scaffold/internal/middleware"
	"heago-scaffold/pkg/serve/controller"

	"github.com/gin-gonic/gin"
)

const NoteReadPermission = "example:notes:read"
const NoteWritePermission = "example:notes:write"

// RegisterNoteRoutes 显式声明 HTTP 方法、权限和控制器方法之间的关系
// protected 已验证身份；每个操作仍须声明权限，读取权限不隐含写入权限
func RegisterNoteRoutes(protected *gin.RouterGroup, notes *controller.NoteController, authentication *auth.Service) {
	group := protected.Group("/examples/notes")
	// 权限值来自代码声明，不允许调用方通过请求参数选择要检查的权限
	// 缺少身份由 protected 拒绝，身份成立但没有权限由 Permission 返回 403
	read := middleware.Permission(authentication, NoteReadPermission)
	write := middleware.Permission(authentication, NoteWritePermission)
	// 所有数据操作仍将 Principal.ID 传到服务与查询；路由权限不替代 owner 隔离
	group.POST("", write, notes.Create)
	group.GET("", read, notes.List)
	group.GET("/:id", read, notes.Get)
	group.PUT("/:id", write, notes.Update)
	group.DELETE("/:id", write, notes.Delete)
}

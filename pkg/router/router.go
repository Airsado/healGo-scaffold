// Package router 依照 gin-scaffold 集中建立路由组，再调用 routes 下的登记函数
package router

import (
	"errors"

	"healgo-scaffold/internal/middleware"
	"healgo-scaffold/pkg/router/routes"
	"healgo-scaffold/pkg/wire"

	"github.com/gin-gonic/gin"
)

// New 集中声明 API 分组和模块挂载，Gin 引擎与全局中间件由 NewEngine 准备
// Container 只在路由层取已构造的对象，Controller 和 Service 不接收整个容器
func New(engine *gin.Engine, container *wire.Container) (err error) {
	// Gin reports conflicting routes by panic. Convert registration failures to a
	// sanitized startup error so bootstrap can close its DB/session resources.
	defer func() {
		if recover() != nil {
			err = errors.New("route registration failed; check route declarations")
		}
	}()
	if engine == nil || container == nil || container.SystemController == nil || container.Auth == nil {
		return errors.New("router dependencies are required")
	}
	// 缺少示例依赖先拒绝，避免已知配置错误时还登记一部分业务路由
	if container.EnableExamples && (container.NoteController == nil || container.RealtimeController == nil) {
		return errors.New("example controllers are required")
	}
	// API 版本前缀是传输协议边界；在 routes 下显式登记资源路径
	public := engine.Group("/api/v1")
	// 分组固定附带认证，模块不需要在每一条保护路由上重复写登录校验
	protected := engine.Group("/api/v1", middleware.Authenticate(container.Auth))
	routes.RegisterSystemRoutes(container, public, protected)
	// 新增业务路由就放在这里，能直接看到 URL、Controller 与启用条件的关系
	if container.EnableExamples {
		routes.RegisterNoteRoutes(container, protected)
		routes.RegisterRealtimeRoutes(container, public, protected)
	}
	return nil
}

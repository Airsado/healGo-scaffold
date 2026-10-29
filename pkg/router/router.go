// Package router 依照 gin-scaffold 集中建立路由组，再调用 routes 下的登记函数
package router

import (
	"context"
	"errors"
	"time"

	"heago-scaffold/internal/db"
	"heago-scaffold/internal/middleware"
	"heago-scaffold/internal/response"
	"heago-scaffold/pkg/router/routes"
	"heago-scaffold/pkg/wire"

	"github.com/gin-gonic/gin"
)

// New 只组装 HTTP 处理链；连接数据库和初始化模型属于启动阶段，不能藏进首次请求
// 接收启动阶段已经构造好的容器；具体 Controller 仍然只依赖自己的 Service
func New(container *wire.Container) (_ *gin.Engine, err error) {
	// Gin reports conflicting routes by panic. Convert registration failures to a
	// sanitized startup error so bootstrap can close its DB/session resources.
	defer func() {
		if recover() != nil {
			err = errors.New("route registration failed; check route declarations")
		}
	}()
	if container == nil || container.SystemController == nil || container.Auth == nil || container.Logger == nil {
		return nil, errors.New("router dependencies are required")
	}
	cfg, deps := container.Config, container.Resources
	// 使用 gin.New 显式选择日志与 recovery，避免框架默认日志输出请求秘密
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	if err := r.SetTrustedProxies(cfg.Security.TrustedProxies); err != nil {
		return nil, errors.New("invalid trusted proxy configuration")
	}
	// 已存在路径的错误方法返回 405，真正不存在的接口返回 404，方便客户端修正请求
	r.HandleMethodNotAllowed = true
	// 顺序决定包裹关系：请求标识先创建，日志包住 recovery，正文限制先于路由解析
	// CORS 在认证前处理浏览器预检；真正的业务请求仍会经过各自的认证链
	r.Use(middleware.RequestID(), middleware.AccessLog(deps.Logger), middleware.Recovery(deps.Logger), middleware.Limits(cfg.Security.MaxBodyBytes, cfg.HTTP.RequestTimeout), middleware.CORS(cfg.Security.CORSOrigins))
	// 存活只说明进程能处理 HTTP；数据库暂时中断不应该触发无意义的容器重启循环
	r.GET("/healthz", func(c *gin.Context) { response.Success(c, gin.H{"alive": true, "application": cfg.App.Name}) })
	r.GET("/readyz", func(c *gin.Context) {
		// 就绪检查单独设置短预算，让负载均衡器及时摘除无法访问数据库的实例
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		if db.Ping(ctx, deps.DB) != nil {
			response.Fail(c, 503, "database_unavailable", "数据库未就绪")
			return
		}
		response.Success(c, gin.H{"ready": true})
	})
	// API 版本前缀是传输协议边界；在 routes 下显式登记资源路径
	public := r.Group("/api/v1")
	// 分组固定附带认证，模块不需要在每一条保护路由上重复写登录校验
	protected := r.Group("/api/v1", middleware.Authenticate(deps.Auth))
	routes.RegisterSystemRoutes(public, protected, container.SystemController, middleware.LoginBudget(cfg.Auth.LoginAttemptsPerMinute))
	// 新增业务路由就放在这里，能直接看到 URL、Controller 与启用条件的关系
	if container.EnableExamples {
		if container.NoteController == nil || container.RealtimeController == nil {
			return nil, errors.New("example controllers are required")
		}
		routes.RegisterNoteRoutes(protected, container.NoteController, deps.Auth)
		routes.RegisterRealtimeRoutes(public, protected, container.RealtimeController, deps.Auth)
	}
	// 兜底也使用统一响应结构，客户端无需额外兼容 Gin 默认的文本错误页
	r.NoRoute(func(c *gin.Context) { response.Fail(c, 404, "not_found", "接口不存在") })
	r.NoMethod(func(c *gin.Context) { response.Fail(c, 405, "method_not_allowed", "请求方法不受支持") })
	return r, nil
}

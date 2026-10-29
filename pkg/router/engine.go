package router

import (
	"context"
	"errors"
	"time"

	"healgo-scaffold/internal/db"
	"healgo-scaffold/internal/middleware"
	"healgo-scaffold/internal/response"
	"healgo-scaffold/pkg/wire"

	"github.com/gin-gonic/gin"
)

// NewEngine 准备全局 HTTP 行为，不登记业务模块或初始化数据库
// 调用方随后执行 New(engine, container)，成功后才能交给 Runtime 监听
func NewEngine(container *wire.Container) (_ *gin.Engine, err error) {
	// 初始化失败不透出库 panic 或配置原文，已打开资源仍由调用方回收
	defer func() {
		if recover() != nil {
			err = errors.New("HTTP engine initialization failed; check configuration")
		}
	}()
	if container == nil || container.Logger == nil {
		return nil, errors.New("HTTP engine dependencies are required")
	}
	cfg, deps := container.Config, container.Resources
	// 显式选择日志与 recovery，避免 Gin 默认日志输出请求秘密
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	if err := engine.SetTrustedProxies(cfg.Security.TrustedProxies); err != nil {
		return nil, errors.New("invalid trusted proxy configuration")
	}
	engine.HandleMethodNotAllowed = true
	// 请求标识先于日志，CORS 先于认证；普通请求超时仍由 Limits 传递
	// WS/SSE 在各自处理器中接管连接生命周期，不在这里添加额外超时包装
	engine.Use(middleware.RequestID(), middleware.AccessLog(deps.Logger), middleware.Recovery(deps.Logger), middleware.Limits(cfg.Security.MaxBodyBytes, cfg.HTTP.RequestTimeout), middleware.CORS(cfg.Security.CORSOrigins))
	// 存活只表示能处理 HTTP，数据库暂时中断不应触发容器重启
	engine.GET("/healthz", func(c *gin.Context) {
		response.Success(c, gin.H{"alive": true, "application": cfg.App.Name})
	})
	engine.GET("/readyz", func(c *gin.Context) {
		// 就绪探针只检查主库，并使用独立短预算及时摘除故障实例
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		if db.Ping(ctx, deps.DB) != nil {
			response.Fail(c, 503, "database_unavailable", "数据库未就绪")
			return
		}
		response.Success(c, gin.H{"ready": true})
	})
	// 未知路径和错误方法也使用统一响应结构，保留 404/405 的区别
	engine.NoRoute(func(c *gin.Context) { response.Fail(c, 404, "not_found", "接口不存在") })
	engine.NoMethod(func(c *gin.Context) { response.Fail(c, 405, "method_not_allowed", "请求方法不受支持") })
	return engine, nil
}

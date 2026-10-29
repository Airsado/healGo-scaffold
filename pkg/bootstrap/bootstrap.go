// Package bootstrap owns schema preparation and the running server lifecycle.
// pkg/wire constructs its dependencies before this package starts listening.
package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"

	"heago-scaffold/configs"
	"heago-scaffold/internal/auth"
	"heago-scaffold/internal/db"
	"heago-scaffold/internal/model"
	"heago-scaffold/internal/settings"
	"heago-scaffold/internal/stream"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type Options struct {
	// 示例只有显式启用时才建表和注册路由；普通模板启动不包含演示接口
	EnableExamples bool
	// Definitions register the key/type/validation contract; persisted values win.
	Settings []settings.Definition
	// The migrate command sets this flag; ordinary startup follows auto_migrate.
	InitializeSchema bool
}

type Runtime struct {
	// Runtime 持有本次启动创建的资源；它不是可随处读取的全局容器
	// 对业务层只传所需依赖，避免业务函数依赖整个启动对象
	Config    configs.Config
	DB        *gorm.DB
	Logger    *slog.Logger
	Auth      *auth.Service
	Settings  *settings.Service
	Streams   *stream.Manager
	Router    *gin.Engine
	cleanup   func()
	closeOnce sync.Once
	drainOnce sync.Once
	drainErr  error
}

// Resources 是本次启动共享的基础设施Controller 仍只接收其需要的构造参数，
// 不能把 Resources 当成请求期间查找任意服务的全局对象
type Resources struct {
	// Database 拥有拓扑及池；DB 保留为固定主库，框架操作不走副本
	Database *db.Cluster
	DB       *gorm.DB
	Logger   *slog.Logger
	Auth     *auth.Service
	Settings *settings.Service
	Streams  *stream.Manager
}

// Components 将已完成注入的基础资源和路由交给唯一的运行时管理
type Components struct {
	Config    configs.Config
	Resources Resources
	Router    *gin.Engine
}

// NewRuntime 绑定注入入口提供的清理函数，避免运行时维护另一份关闭清单
// 手写和 Wire 两种入口都按相反的构造顺序回收依赖
func NewRuntime(components Components, cleanup func()) *Runtime {
	deps := components.Resources
	return &Runtime{Config: components.Config, DB: deps.DB, Logger: deps.Logger,
		Auth: deps.Auth, Settings: deps.Settings, Streams: deps.Streams, Router: components.Router, cleanup: cleanup}
}

// Prepare deliberately expresses startup side effects as one ordered operation.
// Wire orders constructors by type dependencies; it cannot infer business ordering
// such as "initialize tables before seeding" from two independent functions.
func Prepare(ctx context.Context, cfg configs.Config, opts Options, deps Resources) error {
	// 所有模型从一处登记；构造函数只组装对象，不能自己建表或启动后台任务
	models := model.GetAllModels(opts.EnableExamples)
	var err error
	// 建表权和正常服务启动分开生产实例不会因启动就隐式修改 schema
	// 检查仅确认表存在；复杂升级仍须执行经过审查的迁移 SQL
	if opts.InitializeSchema || cfg.Database.AutoMigrate {
		err = db.Initialize(ctx, deps.DB, models)
	} else {
		err = db.CheckSchema(ctx, deps.DB, models)
	}
	if err != nil {
		return err
	}
	// Both seeds only insert missing rows. Restarting is never a password reset
	// or an implicit rollback of settings changed through the administrative API.
	if err = deps.Auth.Bootstrap(ctx, cfg.Bootstrap.AdminUsername, cfg.Bootstrap.AdminPassword); err != nil {
		return err
	}
	if err = deps.Settings.Initialize(ctx); err != nil {
		return err
	}
	return nil
}

// The template ships one generic setting. Future domain keys are
// registered by the application's composition root, never by arbitrary API input.
func DefaultSettings(name string) []settings.Definition {
	// 显示名称允许中文，但仍限制字节长度；防止管理界面被超长文本撑开
	// JSON Marshal 由字符串产生，不能在拼接时把引号变成 JSON 语法
	value, _ := json.Marshal(name)
	return []settings.Definition{{Key: "system.display_name", Description: "系统显示名称", Default: value, Validate: func(v json.RawMessage) error {
		var name string
		if json.Unmarshal(v, &name) != nil || strings.TrimSpace(name) == "" || len(name) > 200 {
			return settings.ErrInvalidInput
		}
		return nil
	}}}
}

// Close 执行注入入口提供的逆依赖顺序清理，并允许安全地重复调用
// 调用者先等待 Serve 完成请求排空，再释放其依赖；没有第二套关闭顺序
func (r *Runtime) Close() {
	r.closeOnce.Do(func() {
		// Serve 已等待过时直接复用结果，不能在 cleanup 阶段重新给一轮停机预算
		// 未启动监听的 Runtime 也允许直接关闭，仍先取消并排空它登记的流
		if r.drain(nil) != nil && r.Logger != nil {
			r.Logger.Error("runtime_drain_exceeded_budget")
		}
		if r.cleanup != nil {
			r.cleanup()
		}
	})
}

// drain 是 Runtime 唯一的停机等待入口HTTP 与 hijack 连接共用同一预算，
// Close 不会在 Serve 超时后再次等待调用者仍须按 Serve 返回后 Close 的顺序使用
func (r *Runtime) drain(server *http.Server) error {
	r.drainOnce.Do(func() {
		// 独立的有界 context 给停机留出时间，不能继承已经取消的应用 context
		ctx, cancel := context.WithTimeout(context.Background(), r.Config.HTTP.ShutdownTimeout)
		defer cancel()
		r.Streams.Close()
		if server != nil {
			// SSE 收到取消后退出，普通 HTTP 请求则有机会在预算内完成
			// 超预算后关闭普通连接；WebSocket 的关闭由流封装负责
			if server.Shutdown(ctx) != nil {
				_ = server.Close()
				r.drainErr = errors.New("HTTP graceful shutdown exceeded its budget")
			}
		}
		if r.Streams.Wait(ctx) != nil && r.drainErr == nil {
			r.drainErr = errors.New("stream graceful shutdown exceeded its budget")
		}
	})
	return r.drainErr
}

// Serve owns the listener and waits for in-flight requests on cancellation.
// Context timeouts inside handlers are cooperative; services must pass them to I/O.
func (r *Runtime) Serve(ctx context.Context) error {
	listener, err := net.Listen("tcp", r.Config.HTTP.Addr)
	if err != nil {
		return errors.New("cannot listen on configured HTTP address")
	}
	server := &http.Server{Handler: r.Router, ReadHeaderTimeout: r.Config.HTTP.ReadHeaderTimeout,
		ReadTimeout: r.Config.HTTP.ReadTimeout, WriteTimeout: r.Config.HTTP.WriteTimeout,
		IdleTimeout: r.Config.HTTP.IdleTimeout}
	// Serve can also return because accepting a connection failed. Close active
	// connections on every exit path before the caller releases DB/session stores.
	defer server.Close()
	done := make(chan error, 1)
	// 缓冲一项让 Serve 结束时总能提交结果，不依赖 select 的分支先后
	// 仅启动一个监听协程，停机时主协程读取结果后才离开
	go func() { done <- server.Serve(listener) }()
	r.Logger.Info("server_started", "application", r.Config.App.Name, "address", listener.Addr().String())
	select {
	case err := <-done:
		// 接收连接发生错误也要排空旧连接，不能只处理应用主动取消的分支
		drainErr := r.drain(server)
		if errors.Is(err, http.ErrServerClosed) {
			return drainErr
		}
		return errors.New("HTTP server stopped unexpectedly")
	case <-ctx.Done():
		err := r.drain(server)
		<-done
		if err != nil {
			return err
		}
		r.Logger.Info("server_stopped")
		return nil
	}
}

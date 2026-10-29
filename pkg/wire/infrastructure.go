package wire

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"gorm.io/gorm"
	"healgo-scaffold/configs"
	"healgo-scaffold/internal/auth"
	"healgo-scaffold/internal/db"
	"healgo-scaffold/internal/logger"
	"healgo-scaffold/internal/settings"
	"healgo-scaffold/internal/stream"
	"healgo-scaffold/pkg/bootstrap"
	"healgo-scaffold/pkg/serve/controller"
)

// 独立类型保证外部资源依赖已经校验过的配置，手写和生成路径复用相同规则
type validatedConfig struct{ configs.Config }

func validateConfig(ctx context.Context, cfg configs.Config) (validatedConfig, error) {
	// 在创建任何 I/O 资源前检查取消，调用方可以中止整个启动过程
	if ctx == nil {
		return validatedConfig{}, errors.New("application context is required")
	}
	if err := ctx.Err(); err != nil {
		return validatedConfig{}, err
	}
	if err := configs.Validate(cfg); err != nil {
		return validatedConfig{}, err
	}
	// 只有本函数能产生 validatedConfig，后续 provider 不重复实现校验
	return validatedConfig{cfg}, nil
}

// slog 的目的地由部署环境接管，业务代码只记录事件和非敏感字段
func provideLogger(cfg validatedConfig) (*slog.Logger, error) { return logger.New(cfg.Logging) }

// 有外部资源的构造器显式返回 cleanup，供两种注入方式处理成功和失败路径
// cleanup 只记录静态事件，不输出可能带密码的驱动错误
func provideDatabaseCluster(ctx context.Context, cfg validatedConfig, log *slog.Logger) (*db.Cluster, func(), error) {
	// db.Open 负责连接池和有界重试；这里负责声明资源的所有权
	database, err := db.OpenCluster(ctx, cfg.Database, log)
	if err != nil {
		return nil, nil, err
	}
	return database, func() {
		// Wire cleanup 签名不返回 error，因此通过静态事件报告关闭失败
		if database.Close() != nil {
			log.Error("database_cleanup_failed")
		}
	}, nil
}

// 所有旧的 *gorm.DB 构造参数继续接主库，不会因为启用副本而改变一致性
func provideDatabase(cluster *db.Cluster) *gorm.DB { return cluster.Primary }

// Auth 依赖 DB，清理它时数据库必须还可用；两种装配都遵守此依赖关系
// 内存会话适用于单实例模板，账号与权限本身仍保存在关系数据库主库
func provideAuth(database *gorm.DB, cfg validatedConfig, log *slog.Logger) (*auth.Service, func(), error) {
	svc, err := auth.New(database, cfg.Auth)
	if err != nil {
		return nil, nil, err
	}
	return svc, func() {
		if svc.Close() != nil {
			log.Error("auth_cleanup_failed")
		}
	}, nil
}

// 部署信息来自 YAML；可编辑设置由 Definition 约定并写入数据库
// 主密钥保留在文件配置中，不能存进它自己负责加密的数据库记录
func provideSettings(database *gorm.DB, cfg validatedConfig, opts bootstrap.Options) (*settings.Service, error) {
	definitions := append(bootstrap.DefaultSettings(cfg.App.Name), opts.Settings...)
	return settings.New(database, cfg.Security.EncryptionKey, definitions)
}

// 参数声明 Auth 依赖，使生成器也能推导出 Streams 应在 Auth 之前关闭
// 共享容量限制保护整个进程，而非为每种协议分别放大连接上限
func provideStreams(_ *auth.Service) (*stream.Manager, func(), error) {
	manager, err := stream.NewManager(128)
	if err != nil {
		return nil, nil, err
	}
	// Runtime 已拥有唯一停机等待预算；这里取消而不再次等待
	return manager, manager.Close, nil
}

// Controller 只收到处理系统 HTTP 所需的服务和会话秒数，不读取整个配置对象
// SessionTTL 已校验为整秒，与 Sa-Token 的过期时间单位一致
func provideSystemController(authentication *auth.Service, configuration *settings.Service, cfg validatedConfig) *controller.SystemController {
	return controller.NewSystemController(authentication, configuration, int64(cfg.Auth.SessionTTL/time.Second))
}

// 来源白名单传给 WS 自身检查；普通 CORS 并不替代 WebSocket 握手校验
// 复制切片避免配置调用方后续修改同一底层数组，悄悄改变已构造对象
func provideRealtimeController(authentication *auth.Service, streams *stream.Manager, cfg validatedConfig) (*controller.RealtimeController, error) {
	return controller.NewRealtimeController(authentication, streams, append([]string(nil), cfg.Security.CORSOrigins...))
}

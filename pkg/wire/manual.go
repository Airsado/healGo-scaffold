package wire

import (
	"context"

	"healgo-scaffold/configs"
	"healgo-scaffold/pkg/bootstrap"
	"healgo-scaffold/pkg/serve/controller"
	"healgo-scaffold/pkg/serve/mapper"
	"healgo-scaffold/pkg/serve/service/impl"
)

// NewContainerManual 提供可选的手写示例，在 buildManual 中增加普通 New 调用，
// 再放入 Container 并登记路由；无需为每个功能增加一套注册协议
func NewContainerManual(ctx context.Context, cfg configs.Config, opts bootstrap.Options) (*Container, error) {
	// build 只组装对象；两条注入路径都必须经过 finish 的初始化屏障
	c, cleanup, err := buildManual(ctx, cfg, opts)
	if err != nil {
		return nil, err
	}
	return finish(ctx, c, cleanup, opts)
}

func buildManual(ctx context.Context, cfg configs.Config, opts bootstrap.Options) (_ *Container, _ func(), err error) {
	// 使用命名 err 让 defer 看到最终返回的错误；不要在后续代码遮蔽它
	// 所有配置先校验，避免打开连接后才发现端口、密钥或超时设置不合法
	validated, err := validateConfig(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	log, err := provideLogger(validated)
	if err != nil {
		return nil, nil, err
	}
	// 当前日志写标准输出，没有独立文件句柄；DB 是第一个需要回收的资源
	cluster, closeDatabase, err := provideDatabaseCluster(ctx, validated, log)
	if err != nil {
		return nil, nil, err
	}
	database := provideDatabase(cluster)
	// 构造途中失败时逆序清理；成功后把相同的清单交给 Container 管理
	cleanups := []func(){closeDatabase}
	// 后创建的对象可能仍依赖前面的对象，因此关闭顺序必须反过来
	cleanup := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	defer func() {
		// 成功时资源归 Container；失败时不返回半成品，立即释放已打开资源
		if err != nil {
			cleanup()
		}
	}()
	// Sa-Token 使用本次启动的 DB 和实例配置，不能另外创建全局认证实例
	authentication, closeAuth, err := provideAuth(database, validated, log)
	if err != nil {
		return nil, nil, err
	}
	cleanups = append(cleanups, closeAuth)
	// 配置定义注册不覆盖数据库已有值；持久化补缺由 finish 统一完成
	// Settings 不拥有额外连接池，无须再次把共享 DB 的 Close 加入清单
	configuration, err := provideSettings(database, validated, opts)
	if err != nil {
		return nil, nil, err
	}
	// 所有长连接共用一个容量登记表，便于 Runtime 停机时统一取消和排空
	// 构造 Manager 不会启动后台任务，未使用 WS/SSE 时也无需特别处理
	streams, closeStreams, err := provideStreams(authentication)
	if err != nil {
		return nil, nil, err
	}
	cleanups = append(cleanups, closeStreams)

	// 这一段就是完整的 Mapper → Service → Controller 显式注入示例
	// 构造示例对象没有 SQL；示例表和路由由 EnableExamples 决定是否启用
	noteMapper, err := mapper.NewNoteMapperWithReader(cluster)
	if err != nil {
		return nil, nil, err
	}
	noteService := impl.NewNoteService(noteMapper)
	// Go 的隐式接口满足让具体 mapper 直接传入服务所需的窄接口
	// Controller 得到服务接口，单测可替换实现而不连接数据库
	noteController := controller.NewNoteController(noteService)
	systemController := provideSystemController(authentication, configuration, validated)
	realtimeController, err := provideRealtimeController(authentication, streams, validated)
	if err != nil {
		return nil, nil, err
	}
	// 只把启动和路由要用的对象放入容器；handler 不接收整个容器
	// 返回 cleanup 而不在此执行，资源必须活到 HTTP 请求排空之后
	return &Container{
		Config: validated.Config,
		Resources: bootstrap.Resources{
			Database: cluster,
			DB:       database,
			Logger:   log,
			Auth:     authentication,
			Settings: configuration,
			Streams:  streams,
		},
		EnableExamples:     opts.EnableExamples,
		NoteMapper:         noteMapper,
		NoteService:        noteService,
		SystemController:   systemController,
		NoteController:     noteController,
		RealtimeController: realtimeController,
	}, cleanup, nil
}

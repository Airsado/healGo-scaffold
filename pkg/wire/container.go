// Package wire 是依赖装配的唯一入口，默认 Google Wire，亦可选择手写构造
// Container 只在启动/路由层传递，Controller 和 Service 使用明确的构造参数
package wire

import (
	"context"
	"sync"

	"healgo-scaffold/configs"
	"healgo-scaffold/pkg/bootstrap"
	"healgo-scaffold/pkg/serve/controller"
	"healgo-scaffold/pkg/serve/mapper"
	"healgo-scaffold/pkg/serve/service"
)

type Container struct {
	// 启动与路由共享同一份已校验配置，不从环境或请求中临时改变连接信息
	Config configs.Config
	bootstrap.Resources
	// 开关控制 schema 与路由，不影响只绑定依赖的示例构造函数
	EnableExamples     bool
	NoteMapper         *mapper.NoteMapper
	NoteService        service.NoteService
	SystemController   *controller.SystemController
	NoteController     *controller.NoteController
	RealtimeController *controller.RealtimeController
	// 清理函数由选中的注入路径提供；容器只负责保证它执行一次
	// Wire 只装配业务依赖；生命周期字段由 finish 和 Close 管理
	cleanup   func()    `wire:"-"`
	closeOnce sync.Once `wire:"-"`
}

// NewContainer 是默认 Wire 入口，构造后统一执行初始化和资源接管
// 生成代码仍在编译期检查依赖，没有请求期间反射查找的服务定位器
func NewContainer(ctx context.Context, cfg configs.Config, opts bootstrap.Options) (*Container, error) {
	c, cleanup, err := buildWithWire(ctx, cfg, opts)
	if err != nil {
		return nil, err
	}
	return finish(ctx, c, cleanup, opts)
}

func finish(ctx context.Context, c *Container, cleanup func(), opts bootstrap.Options) (*Container, error) {
	// 先接管资源，再执行可能失败的 I/O，失败路径才能使用同一个 Close
	c.cleanup = cleanup
	// 对象先构造，模型再初始化，最后才由 cmd 调用 router.New 并启动监听
	// 初始化失败同样回收已经打开的资源，不将失败推迟到第一个 HTTP 请求
	if err := bootstrap.Prepare(ctx, c.Config, opts, c.Resources); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// Close 按 Streams→Auth→DB 释放资源，并发/重复调用也只执行一次
// 开始监听后，须先由 Runtime.Serve 排空请求再调用；CLI 已固定这一顺序
func (c *Container) Close() {
	c.closeOnce.Do(func() {
		if c.cleanup != nil {
			c.cleanup()
		}
	})
}

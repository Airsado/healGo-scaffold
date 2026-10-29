//go:build wireinject

package wire

import (
	"context"
	"github.com/google/wire"
	"healgo-scaffold/configs"
	"healgo-scaffold/pkg/bootstrap"
)

// buildWithWire 生成的只是普通 Go 构造调用；构造完成后的建表、种子初始化
// 统一由 finish 执行，与可选手写路径保持同一启动规则
func buildWithWire(ctx context.Context, cfg configs.Config, opts bootstrap.Options) (*Container, func(), error) {
	// Struct 按字段类型装配，不必每次新增 Controller 都维护一长串构造参数
	// Config 来自入口参数；Options 只公开需要注入的示例开关
	panic(wire.Build(
		AllProviders,
		wire.Struct(new(bootstrap.Resources), "*"),
		wire.FieldsOf(new(bootstrap.Options), "EnableExamples"),
		wire.Struct(new(Container), "*"),
	))
}

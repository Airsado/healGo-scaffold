//go:build wireinject

package wire

import (
	"context"
	"github.com/google/wire"
	"heago-scaffold/configs"
	"heago-scaffold/pkg/bootstrap"
)

// buildWithWire 生成的只是普通 Go 构造调用；构造完成后的建表、种子初始化
// 统一由 finish 执行，与默认手写路径保持同一启动规则
func buildWithWire(ctx context.Context, cfg configs.Config, opts bootstrap.Options) (*Container, func(), error) {
	panic(wire.Build(AllProviders))
}

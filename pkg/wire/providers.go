//go:build wireinject

package wire

import (
	"github.com/google/wire"
	"healgo-scaffold/pkg/serve/controller"
	"healgo-scaffold/pkg/serve/mapper"
	"healgo-scaffold/pkg/serve/service"
	"healgo-scaffold/pkg/serve/service/impl"
)

// 依照 gin-scaffold 将 provider 按层集中，默认由 Wire 生成完整容器
// 这里声明接口与实现的关系，业务代码不取全局变量或导入 Wire DSL
var InfrastructureProviders = wire.NewSet(
	validateConfig,
	provideLogger,
	provideDatabaseCluster,
	provideDatabase,
	provideAuth,
	provideSettings,
	provideStreams,
)

var MapperProviders = wire.NewSet(
	mapper.NewNoteMapperWithReader,
	wire.Bind(new(service.NoteMapper), new(*mapper.NoteMapper)),
)

// NewNoteService 返回 service.NoteService 接口，Controller 只依赖用例契约
var ServiceProviders = wire.NewSet(
	impl.NewNoteService,
)
var ControllerProviders = wire.NewSet(
	controller.NewNoteController,
	provideSystemController,
	provideRealtimeController,
)

var AllProviders = wire.NewSet(
	InfrastructureProviders,
	MapperProviders,
	ServiceProviders,
	ControllerProviders,
)

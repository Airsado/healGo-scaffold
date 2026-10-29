//go:build wireinject

package wire

import (
	"github.com/google/wire"
	"heago-scaffold/pkg/serve/controller"
	"heago-scaffold/pkg/serve/mapper"
	"heago-scaffold/pkg/serve/service"
	"heago-scaffold/pkg/serve/service/impl"
)

// 依照 gin-scaffold 将 provider 按层集中默认手写入口见 manual.go；
// 选择生成注入时，这里负责说明接口与实现的关系，不在业务代码中取全局变量
var InfrastructureProviders = wire.NewSet(validateConfig, provideLogger, provideDatabaseCluster, provideDatabase,
	provideAuth, provideSettings, provideStreams)

var MapperProviders = wire.NewSet(mapper.NewNoteMapperWithReader,
	wire.Bind(new(service.NoteMapper), new(*mapper.NoteMapper)))

// NewNoteService 返回 service.NoteService 接口，Controller 只依赖用例契约
var ServiceProviders = wire.NewSet(impl.NewNoteService)
var ControllerProviders = wire.NewSet(controller.NewNoteController, provideSystemController, provideRealtimeController)

var AllProviders = wire.NewSet(InfrastructureProviders, MapperProviders,
	ServiceProviders, ControllerProviders, newContainer)

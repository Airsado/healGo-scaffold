package router

import (
	"log/slog"
	"strings"
	"testing"

	"healgo-scaffold/internal/auth"
	"healgo-scaffold/pkg/bootstrap"
	"healgo-scaffold/pkg/serve/controller"
	"healgo-scaffold/pkg/wire"

	"github.com/gin-gonic/gin"
)

func routeTestContainer() *wire.Container {
	// 零值认证服务只处理无凭证拒绝；这些用例不会访问会话存储或数据库
	authentication := &auth.Service{}
	return &wire.Container{Config: testConfig(),
		Resources:        bootstrap.Resources{Auth: authentication, Logger: slog.Default()},
		SystemController: controller.NewSystemController(authentication, nil, 3600)}
}

func TestEngineAndRouteRegistrationAreSeparateStartupSteps(t *testing.T) {
	container := routeTestContainer()
	engine, err := NewEngine(container)
	if err != nil {
		t.Fatal(err)
	}
	// 引擎只准备全局 HTTP 行为；尚未登记的登录接口应返回 404
	request(t, engine, "GET", "/healthz", "", "", 200)
	request(t, engine, "GET", "/readyz", "", "", 503)
	request(t, engine, "POST", "/api/v1/auth/login", `{}`, "", 404)
	if err := New(engine, container); err != nil {
		t.Fatal(err)
	}
	// 登记后可见接口，非法输入与无凭证均在访问数据库之前被拒绝
	request(t, engine, "POST", "/api/v1/auth/login", `{}`, "", 400)
	request(t, engine, "GET", "/api/v1/auth/me", "", "", 401)
	// 同一个引擎不得重复挂载模块，Gin 的路径等 panic 内容不能泄漏到启动错误
	if err := New(engine, container); err == nil || err.Error() != "route registration failed; check route declarations" {
		t.Fatalf("repeated registration must return a sanitized startup error: %v", err)
	}
}

func TestSystemRouteLoginBudgetPersistsAcrossRequests(t *testing.T) {
	container := routeTestContainer()
	container.Config.Auth.LoginAttemptsPerMinute = 1
	engine := newTestRouter(t, container)
	// 格式错误仍计入尝试预算，连续请求共享登记时创建的同一个闭包
	request(t, engine, "POST", "/api/v1/auth/login", `{}`, "", 400)
	w, output := request(t, engine, "POST", "/api/v1/auth/login", `{}`, "", 429)
	if w.Header().Get("Retry-After") != "60" || output.Error == nil || output.Error.Code != "login_rate_limited" {
		t.Fatal("login route lost its configured retry budget")
	}
	// 另一个应用实例独立持有预算，不能通过全局路由状态互相影响
	other := newTestRouter(t, container)
	request(t, other, "POST", "/api/v1/auth/login", `{}`, "", 400)
}

func TestEngineRejectsMissingDependenciesAndInvalidProxyWithoutEcho(t *testing.T) {
	for _, container := range []*wire.Container{nil, {Config: testConfig()}} {
		if engine, err := NewEngine(container); err == nil || engine != nil {
			t.Fatal("engine accepted missing dependencies")
		}
	}
	container := routeTestContainer()
	const privateProxy = "invalid-private-proxy-value"
	container.Config.Security.TrustedProxies = []string{privateProxy}
	engine, err := NewEngine(container)
	if err == nil || engine != nil || strings.Contains(err.Error(), privateProxy) {
		t.Fatal("invalid proxy was accepted or echoed into startup error")
	}
}

func TestRouteRegistrationRejectsMissingDependencies(t *testing.T) {
	valid := routeTestContainer()
	withoutAuth := routeTestContainer()
	withoutAuth.Auth = nil
	withoutController := routeTestContainer()
	withoutController.SystemController = nil
	missingExamples := routeTestContainer()
	missingExamples.EnableExamples = true
	for _, tc := range []struct {
		name      string
		engine    *gin.Engine
		container *wire.Container
	}{
		{name: "engine", container: valid},
		{name: "container", engine: gin.New()},
		{name: "authentication", engine: gin.New(), container: withoutAuth},
		{name: "system controller", engine: gin.New(), container: withoutController},
		{name: "example controllers", engine: gin.New(), container: missingExamples},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := New(tc.engine, tc.container); err == nil {
				t.Fatal("route registration accepted missing dependencies")
			}
		})
	}
}

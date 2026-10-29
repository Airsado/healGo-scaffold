package router

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"healgo-scaffold/internal/testutil"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"healgo-scaffold/configs"
	"healgo-scaffold/internal/auth"
	"healgo-scaffold/internal/middleware"
	"healgo-scaffold/internal/response"
	"healgo-scaffold/internal/settings"
	"healgo-scaffold/pkg/bootstrap"
	"healgo-scaffold/pkg/serve/controller"
	"healgo-scaffold/pkg/wire"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const adminPassword = "http-admin-password-5c0d7"
const viewerPassword = "http-viewer-password-a74f9"

type wireEnvelope struct {
	Data      json.RawMessage `json:"data"`
	Error     *response.Error `json:"error"`
	RequestID string          `json:"requestId"`
	TimeStamp int64           `json:"timeStamp"`
}

// registerProbeRoutes 用普通函数演示路由登记，不引入额外模块协议
func registerProbeRoutes(public, protected *gin.RouterGroup, authentication *auth.Service) {
	public.GET("/probe/public", func(c *gin.Context) { response.Success(c, gin.H{"public": true}) })
	public.POST("/probe/echo", func(c *gin.Context) {
		var input struct {
			Text string `json:"text" binding:"required"`
		}
		if response.BindJSON(c, &input) {
			response.Success(c, gin.H{"text": input.Text})
		}
	})
	public.GET("/probe/panic/:id", func(c *gin.Context) { panic("sensitive-panic-5c37e") })
	protected.GET("/probe/read", middleware.Permission(authentication, "probe:read"), func(c *gin.Context) {
		principal, ok := middleware.Principal(c)
		response.Success(c, gin.H{"authenticated": ok, "username": principal.Username})
	})
	protected.GET("/probe/operator", middleware.Role(authentication, "operator"), func(c *gin.Context) {
		response.Success(c, gin.H{"operator": true})
	})
}

type fixture struct {
	db       *gorm.DB
	auth     *auth.Service
	settings *settings.Service
	router   *gin.Engine
	logs     *bytes.Buffer
	cfg      configs.Config
}

func testConfig() configs.Config {
	return configs.Config{
		App:      configs.App{Name: "http-test", Environment: "test"},
		HTTP:     configs.HTTP{RequestTimeout: 5 * time.Second},
		Auth:     configs.Auth{SessionTTL: time.Hour, MaxSessions: 5, LoginAttemptsPerMinute: 100},
		Security: configs.Security{MaxBodyBytes: 1024, CORSOrigins: []string{"https://console.example"}},
	}
}

func testDefinitions() []settings.Definition {
	return []settings.Definition{
		{Key: "system.site_name", Description: "Site name", Default: json.RawMessage(`"Initial"`)},
		{Key: "system.maintenance", Default: json.RawMessage(`false`)},
		{Key: "test.credentials", Secret: true, Default: json.RawMessage(`""`)},
	}
}

func isolatedDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	database := testutil.OpenDatabase(t)
	if err := database.AutoMigrate(append(auth.Models(), settings.Models()...)...); err != nil {
		t.Fatal("initialize isolated test tables")
	}
	return database
}

func newTestRouter(t *testing.T, container *wire.Container) *gin.Engine {
	t.Helper()
	engine, err := NewEngine(container)
	if err != nil {
		t.Fatal(err)
	}
	if err := New(engine, container); err != nil {
		t.Fatal(err)
	}
	return engine
}

func newFixture(t *testing.T, database *gorm.DB) *fixture {
	t.Helper()
	if database == nil {
		database = isolatedDatabase(t)
	}
	cfg := testConfig()
	authentication, err := auth.New(database, cfg.Auth)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authentication.Close() })
	if err := authentication.Bootstrap(context.Background(), "admin", adminPassword); err != nil {
		t.Fatal(err)
	}
	configuration, err := settings.New(database, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)), testDefinitions())
	if err != nil || configuration.Initialize(context.Background()) != nil {
		t.Fatal("initialize HTTP settings service failed")
	}
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	engine := newTestRouter(t, &wire.Container{Config: cfg, Resources: bootstrap.Resources{DB: database, Auth: authentication, Settings: configuration, Logger: log}, SystemController: controller.NewSystemController(authentication, configuration, int64(cfg.Auth.SessionTTL/time.Second))})
	registerProbeRoutes(engine.Group("/api/v1"), engine.Group("/api/v1", middleware.Authenticate(authentication)), authentication)
	return &fixture{db: database, auth: authentication, settings: configuration, router: engine, logs: logs, cfg: cfg}
}

// 就绪只依赖主库连接；主库不可用时摘流，但存活接口继续返回200，避免重启循环
// 副本失联不影响主库就绪的行为由真实复制故障集成另行覆盖
func TestReadinessFailsWithPrimaryWhileLivenessStaysAvailable(t *testing.T) {
	f := newFixture(t, nil)
	request(t, f.router, "GET", "/readyz", "", "", 200)
	pool, err := f.db.DB()
	if err != nil {
		t.Fatal("obtain primary pool")
	}
	if err := pool.Close(); err != nil {
		t.Fatal("close primary pool")
	}
	request(t, f.router, "GET", "/readyz", "", "", 503)
	request(t, f.router, "GET", "/healthz", "", "", 200)
}

func request(t *testing.T, engine http.Handler, method, path, body, token string, status int) (*httptest.ResponseRecorder, wireEnvelope) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("%s %s: status=%d want=%d body=%s", method, path, w.Code, status, w.Body.String())
	}
	var envelope wireEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response envelope: %v", err)
	}
	if envelope.RequestID == "" || envelope.RequestID != w.Header().Get("X-Request-ID") || envelope.TimeStamp <= 0 {
		t.Fatal("response request ID/timestamp contract missing")
	}
	if _, err := uuid.Parse(envelope.RequestID); err != nil {
		t.Fatal("request ID is not server-generated UUID")
	}
	if status >= 400 && (envelope.Error == nil || envelope.Error.Code == "" || len(envelope.Data) != 0) {
		t.Fatal("error response did not use the error envelope")
	}
	if status < 400 && (envelope.Error != nil || len(envelope.Data) == 0) {
		t.Fatal("success response did not use the data envelope")
	}
	return w, envelope
}

func dataAs[T any](t *testing.T, envelope wireEnvelope) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(envelope.Data, &value); err != nil {
		t.Fatalf("decode response data: %v", err)
	}
	return value
}

func login(t *testing.T, engine http.Handler, username, password string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	_, envelope := request(t, engine, http.MethodPost, "/api/v1/auth/login", string(body), "", 200)
	data := dataAs[struct {
		Token     string         `json:"token"`
		TokenType string         `json:"token_type"`
		ExpiresIn int64          `json:"expires_in"`
		User      auth.Principal `json:"user"`
	}](t, envelope)
	if data.Token == "" || data.TokenType != "Bearer" || data.ExpiresIn != 3600 || data.User.Username != username {
		t.Fatal("login response contract invalid")
	}
	return data.Token
}

func TestHTTPAuthenticationAuthorizationAndExplicitRoutes(t *testing.T) {
	f := newFixture(t, nil)
	request(t, f.router, "GET", "/healthz", "", "", 200)
	request(t, f.router, "GET", "/readyz", "", "", 200)
	request(t, f.router, "GET", "/api/v1/probe/public", "", "", 200)
	request(t, f.router, "GET", "/api/v1/system/settings", "", "", 401)
	request(t, f.router, "POST", "/api/v1/auth/login", `{"username":"admin","password":"wrong-password-48b"}`, "", 401)
	request(t, f.router, "POST", "/api/v1/auth/login", `{"username":"admin","password":"`+adminPassword+`","typo":true}`, "", 400)
	adminToken := login(t, f.router, "admin", adminPassword)
	request(t, f.router, "GET", "/api/v1/auth/me", "", adminToken, 200)

	// Cookie and query credentials are intentionally ignored, even when valid.
	r := httptest.NewRequest("GET", "/api/v1/auth/me?token="+adminToken, nil)
	r.AddCookie(&http.Cookie{Name: "Authorization", Value: adminToken})
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("cookie/query token granted authentication")
	}
	r = httptest.NewRequest("GET", "/api/v1/auth/me", nil)
	r.Header.Add("Authorization", "Bearer "+adminToken)
	r.Header.Add("Authorization", "Bearer "+adminToken)
	w = httptest.NewRecorder()
	f.router.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("duplicate Authorization headers accepted")
	}
	request(t, f.router, "PUT", "/api/v1/system/roles/viewer", `{"permissions":["system:settings:read","probe:read"]}`, adminToken, 200)
	_, created := request(t, f.router, "POST", "/api/v1/system/users", `{"username":"viewer","password":"`+viewerPassword+`","roles":["viewer"]}`, adminToken, 201)
	viewer := dataAs[auth.Principal](t, created)
	if viewer.ID == 0 || viewer.Username != "viewer" || len(viewer.Roles) != 1 {
		t.Fatal("create-user response invalid")
	}
	viewerToken := login(t, f.router, "viewer", viewerPassword)
	request(t, f.router, "GET", "/api/v1/system/settings", "", viewerToken, 200)
	request(t, f.router, "PUT", "/api/v1/system/settings/system.site_name", `{"value":"Forbidden","revision":1}`, viewerToken, 403)
	request(t, f.router, "GET", "/api/v1/system/users", "", viewerToken, 403)
	request(t, f.router, "GET", "/api/v1/probe/read", "", viewerToken, 200)
	request(t, f.router, "GET", "/api/v1/probe/operator", "", viewerToken, 403)

	// Policy changes must affect an already-issued token, not only new logins.
	request(t, f.router, "PUT", "/api/v1/system/roles/operator", `{"permissions":["probe:read"]}`, adminToken, 200)
	viewerPath := "/api/v1/system/users/" + strconv.FormatUint(viewer.ID, 10)
	request(t, f.router, "PUT", viewerPath+"/roles", `{"roles":["operator"]}`, adminToken, 200)
	request(t, f.router, "GET", "/api/v1/probe/operator", "", viewerToken, 200)
	request(t, f.router, "GET", "/api/v1/system/settings", "", viewerToken, 403)
	request(t, f.router, "PUT", viewerPath+"/enabled", `{"enabled":false}`, adminToken, 200)
	request(t, f.router, "GET", "/api/v1/auth/me", "", viewerToken, 401)
	request(t, f.router, "PUT", viewerPath+"/enabled", `{"enabled":true}`, adminToken, 200)
	request(t, f.router, "GET", "/api/v1/auth/me", "", viewerToken, 401)
	viewerToken = login(t, f.router, "viewer", viewerPassword)
	request(t, f.router, "POST", "/api/v1/auth/logout", "", viewerToken, 200)
	request(t, f.router, "GET", "/api/v1/auth/me", "", viewerToken, 401)
	request(t, f.router, "POST", "/api/v1/auth/logout", "", adminToken, 200)
	request(t, f.router, "GET", "/api/v1/auth/me", "", adminToken, 401)
	for _, secret := range []string{adminPassword, viewerPassword, adminToken, viewerToken} {
		if strings.Contains(f.logs.String(), secret) {
			t.Fatal("credential leaked in HTTP log")
		}
	}
}

func TestHTTPPasswordLengthUsesUTF8Bytes(t *testing.T) {
	f := newFixture(t, nil)
	adminToken := login(t, f.router, "admin", adminPassword)
	// Four Chinese characters meet bcrypt's 12-byte lower bound. Gin's string
	// min validator counts runes, so it must not override the service contract.
	request(t, f.router, "POST", "/api/v1/system/users", `{"username":"unicode_user","password":"你好世界","roles":[]}`, adminToken, 201)
	token := login(t, f.router, "unicode_user", "你好世界")
	request(t, f.router, "PUT", "/api/v1/auth/password", `{"old_password":"你好世界","new_password":"天地玄黄"}`, token, 200)
	request(t, f.router, "GET", "/api/v1/auth/me", "", token, 401)
	login(t, f.router, "unicode_user", "天地玄黄")
	request(t, f.router, "POST", "/api/v1/system/users", `{"username":"short_password","password":"你好世","roles":[]}`, adminToken, 400)
}

func TestHTTPSettingsAuditSecretsAndRestartPersistence(t *testing.T) {
	f := newFixture(t, nil)
	token := login(t, f.router, "admin", adminPassword)
	_, envelope := request(t, f.router, "GET", "/api/v1/system/settings", "", token, 200)
	listed := dataAs[struct {
		Items []settings.View `json:"items"`
	}](t, envelope)
	if len(listed.Items) != 3 {
		t.Fatal("registered settings list incomplete")
	}
	for _, item := range listed.Items {
		if item.Secret && (item.Configured || len(item.Value) != 0) {
			t.Fatal("empty secret list state invalid")
		}
	}
	_, envelope = request(t, f.router, "GET", "/api/v1/system/settings/system.site_name", "", token, 200)
	initial := dataAs[settings.View](t, envelope)
	if initial.Revision != 1 || string(initial.Value) != `"Initial"` {
		t.Fatal("initial setting response invalid")
	}
	_, envelope = request(t, f.router, "PUT", "/api/v1/system/settings/system.site_name", `{"value":"Saved by HTTP","revision":1}`, token, 200)
	if updated := dataAs[settings.View](t, envelope); updated.Revision != 2 || string(updated.Value) != `"Saved by HTTP"` {
		t.Fatal("setting write response invalid")
	}
	_, conflict := request(t, f.router, "PUT", "/api/v1/system/settings/system.site_name", `{"value":"Stale edit","revision":1}`, token, 409)
	if conflict.Error.Code != "revision_conflict" {
		t.Fatal("stale update did not return revision_conflict")
	}
	for _, body := range []string{`{"value":true,"revision":2}`, `{"value":null,"revision":2}`, `{"revision":2}`, `{"value":"Typo","revision":2,"expectedRevision":2}`, `{"value":"Wrong revision","revision":0}`} {
		request(t, f.router, "PUT", "/api/v1/system/settings/system.site_name", body, token, 400)
	}
	request(t, f.router, "GET", "/api/v1/system/settings/unknown.key", "", token, 404)
	request(t, f.router, "PUT", "/api/v1/system/settings/unknown.key", `{"value":"Unknown","revision":1}`, token, 404)
	const secret = "http-test-secret-6fd719cf"
	secretPath := "/api/v1/system/settings/test.credentials"
	w, envelope := request(t, f.router, "PUT", secretPath, `{"value":"`+secret+`","revision":1}`, token, 200)
	view := dataAs[settings.View](t, envelope)
	if !view.Configured || len(view.Value) != 0 || strings.Contains(w.Body.String(), secret) || strings.Contains(string(envelope.Data), `"value"`) {
		t.Fatal("secret write response is not redacted")
	}
	request(t, f.router, "PUT", secretPath, `{"revision":2}`, token, 400)
	_, envelope = request(t, f.router, "PUT", secretPath, `{"value":null,"revision":2}`, token, 200)
	if dataAs[settings.View](t, envelope).Configured {
		t.Fatal("explicit clear left secret configured")
	}
	request(t, f.router, "PUT", secretPath, `{"value":"`+secret+`","revision":3}`, token, 200)
	w, envelope = request(t, f.router, "GET", "/api/v1/system/setting-audits?limit=20&offset=0", "", token, 200)
	audited := dataAs[struct {
		Items []settings.Audit `json:"items"`
		Total int64            `json:"total"`
	}](t, envelope)
	if audited.Total != 4 || len(audited.Items) != 4 || strings.Contains(w.Body.String(), secret) || strings.Contains(string(envelope.Data), `"value"`) {
		t.Fatal("audit missing accepted writes or exposing values")
	}
	for _, audit := range audited.Items {
		if audit.Actor != "1" || audit.NewRevision != audit.OldRevision+1 {
			t.Fatal("audit actor/revision is not server-owned")
		}
	}
	request(t, f.router, "GET", "/api/v1/system/setting-audits?limit=101", "", token, 400)
	request(t, f.router, "GET", "/api/v1/system/setting-audits?offset=-1", "", token, 400)

	// Reconstructing both services represents a process restart: persisted
	// values remain, while the memory-backed authentication session ends.
	restarted := newFixture(t, f.db)
	request(t, restarted.router, "GET", "/api/v1/auth/me", "", token, 401)
	newToken := login(t, restarted.router, "admin", adminPassword)
	_, envelope = request(t, restarted.router, "GET", "/api/v1/system/settings/system.site_name", "", newToken, 200)
	if persisted := dataAs[settings.View](t, envelope); persisted.Revision != 2 || string(persisted.Value) != `"Saved by HTTP"` {
		t.Fatal("ordinary setting did not survive restart")
	}
	w, envelope = request(t, restarted.router, "GET", secretPath, "", newToken, 200)
	if persisted := dataAs[settings.View](t, envelope); !persisted.Configured || persisted.Revision != 4 || len(persisted.Value) != 0 || strings.Contains(w.Body.String(), secret) {
		t.Fatal("secret restart metadata invalid or leaking")
	}
	value, err := restarted.settings.Value(context.Background(), "test.credentials")
	if err != nil || string(value) != `"`+secret+`"` {
		t.Fatal("internal secret accessor did not decrypt after restart")
	}
	var stored struct{ Payload []byte }
	if err := f.db.Table("sys_settings").Select("payload").Where(clause.Eq{Column: "key", Value: "test.credentials"}).Take(&stored).Error; err != nil {
		t.Fatal("read stored encrypted payload")
	}
	if bytes.Contains(stored.Payload, []byte(secret)) {
		t.Fatal("database contains plaintext secret")
	}
	if strings.Contains(f.logs.String()+restarted.logs.String(), secret) {
		t.Fatal("secret leaked to HTTP access log")
	}
}

func TestHTTPBoundariesCORSAndSanitizedRecovery(t *testing.T) {
	// Public-route boundaries need no database; they always run, even when
	// integration credentials are not supplied to a developer's unit-test run.
	cfg := testConfig()
	logs := &bytes.Buffer{}
	authentication := &auth.Service{}
	engine := newTestRouter(t, &wire.Container{Config: cfg, Resources: bootstrap.Resources{Auth: authentication, Logger: slog.New(slog.NewJSONHandler(logs, nil))}, SystemController: controller.NewSystemController(authentication, nil, 3600)})
	registerProbeRoutes(engine.Group("/api/v1"), engine.Group("/api/v1", middleware.Authenticate(authentication)), authentication)
	request(t, engine, "GET", "/not-registered?secret=sensitive-query-71ab", "", "", 404)
	request(t, engine, "POST", "/healthz", "", "", 405)
	request(t, engine, "POST", "/api/v1/probe/echo", `{"text":"ok","unknown":"sensitive-body-29d"}`, "", 400)
	request(t, engine, "POST", "/api/v1/probe/echo", `{"text":"ok"} {"text":"trailing"}`, "", 400)
	request(t, engine, "POST", "/api/v1/probe/echo", `{"text":3}`, "", 400)
	request(t, engine, "POST", "/api/v1/probe/echo", `{"text":"`+strings.Repeat("x", 2000)+`"}`, "", 413)
	for _, body := range []string{`{"text":"` + strings.Repeat("x", 2000) + `"}`, `{"text":"ok"}` + strings.Repeat(" ", 2000)} {
		r := httptest.NewRequest("POST", "/api/v1/probe/echo", strings.NewReader(body))
		r.ContentLength = -1 // Simulate a chunked body without an announced size.
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, r)
		if w.Code != 413 {
			t.Fatalf("chunked oversized body status=%d want=413", w.Code)
		}
	}
	for _, contentType := range []string{"", "text/plain"} {
		r := httptest.NewRequest("POST", "/api/v1/probe/echo", strings.NewReader(`{"text":"ok"}`))
		r.Header.Set("Content-Type", contentType)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, r)
		if w.Code != 415 {
			t.Fatal("non-JSON media type accepted")
		}
	}
	for _, test := range []struct {
		origin, method, headers string
		status                  int
	}{
		{"https://console.example", "PUT", "Authorization, Content-Type", 204},
		{"https://forbidden.example", "PUT", "Authorization", 403},
		{"https://console.example", "TRACE", "Authorization", 403},
		{"https://console.example", "PUT", "X-Unapproved", 403},
	} {
		r := httptest.NewRequest("OPTIONS", "/api/v1/system/settings/system.site_name", nil)
		r.Header.Set("Origin", test.origin)
		r.Header.Set("Access-Control-Request-Method", test.method)
		r.Header.Set("Access-Control-Request-Headers", test.headers)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, r)
		if w.Code != test.status || w.Header().Get("Access-Control-Allow-Credentials") != "" {
			t.Fatalf("CORS status=%d want=%d or cookie credentials enabled", w.Code, test.status)
		}
		if test.status == 204 && (w.Header().Get("Access-Control-Allow-Origin") != test.origin || w.Body.Len() != 0) {
			t.Fatal("allowed preflight response invalid")
		}
	}
	r := httptest.NewRequest("GET", "/api/v1/probe/panic/sensitive-path-bc20?key=sensitive-query-71ab", strings.NewReader("sensitive-body-29d"))
	r.Header.Set("Authorization", "Bearer sensitive-token-3e8a")
	r.Header.Set("X-Request-ID", "sensitive-request-id-6cad")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, r)
	if w.Code != 500 || w.Header().Get("X-Request-ID") == "sensitive-request-id-6cad" {
		t.Fatal("panic recovery or server-owned request ID failed")
	}
	for _, secret := range []string{"sensitive-panic-5c37e", "sensitive-path-bc20", "sensitive-query-71ab", "sensitive-body-29d", "sensitive-token-3e8a", "sensitive-request-id-6cad"} {
		if strings.Contains(logs.String()+w.Body.String(), secret) {
			t.Fatal("panic, URL, body, token or incoming request ID leaked")
		}
	}
	if !strings.Contains(logs.String(), "/api/v1/probe/panic/:id") {
		t.Fatal("sanitized registered route missing from panic log")
	}
	for _, header := range []string{"X-Content-Type-Options", "Referrer-Policy", "Cache-Control"} {
		if w.Header().Get(header) == "" {
			t.Fatalf("security response header missing: %s", header)
		}
	}
}

// 固定分层以后，router.go 是挂载位置；这里验证入口开关确实覆盖所有示例
// 只检查路由表，不执行数据库用例；CRUD 和流协议由 routes 集成测试验证
func TestExampleRoutesRequireExplicitEnablement(t *testing.T) {
	authentication := &auth.Service{}
	c := &wire.Container{Config: testConfig(),
		Resources:        bootstrap.Resources{Auth: authentication, Logger: slog.Default()},
		SystemController: controller.NewSystemController(authentication, nil, 3600),
		NoteController:   &controller.NoteController{}, RealtimeController: &controller.RealtimeController{}}
	for _, enabled := range []bool{false, true} {
		c.EnableExamples = enabled
		engine := newTestRouter(t, c)
		registered := map[string]bool{}
		for _, route := range engine.Routes() {
			registered[route.Method+" "+route.Path] = true
		}
		for _, route := range []string{
			"GET /api/v1/examples/notes", "POST /api/v1/examples/notes",
			"GET /api/v1/examples/notes/:id", "PUT /api/v1/examples/notes/:id", "DELETE /api/v1/examples/notes/:id",
			"GET /api/v1/examples/realtime/sse", "POST /api/v1/examples/realtime/tickets", "GET /api/v1/examples/realtime/ws",
		} {
			if registered[route] != enabled {
				t.Fatalf("example route %s: enabled=%v registered=%v", route, enabled, registered[route])
			}
		}
	}
}

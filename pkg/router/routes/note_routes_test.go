package routes_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"healgo-scaffold/internal/testutil"
	"io"
	"log/slog"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"healgo-scaffold/configs"
	"healgo-scaffold/internal/auth"
	"healgo-scaffold/internal/middleware"
	model "healgo-scaffold/internal/model/note"
	"healgo-scaffold/internal/response"
	"healgo-scaffold/internal/settings"
	"healgo-scaffold/pkg/bootstrap"
	frameworkrouter "healgo-scaffold/pkg/router"
	"healgo-scaffold/pkg/router/routes"
	"healgo-scaffold/pkg/serve/controller"
	"healgo-scaffold/pkg/serve/mapper"
	"healgo-scaffold/pkg/serve/service/impl"
	"healgo-scaffold/pkg/vo"
	"healgo-scaffold/pkg/wire"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const testPassword = "note-integration-password-47b9"

func testDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	database := testutil.OpenDatabase(t)
	return database
}

type envelope struct {
	Data  json.RawMessage `json:"data"`
	Error *response.Error `json:"error"`
}

func request(t *testing.T, engine *gin.Engine, method, path, body, token string, status int) envelope {
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
		t.Fatalf("%s %s: got %d want %d: %s", method, path, w.Code, status, w.Body.String())
	}
	var output envelope
	if err := json.Unmarshal(w.Body.Bytes(), &output); err != nil {
		t.Fatal("decode framework envelope", err)
	}
	if status >= 400 && output.Error == nil {
		t.Fatal("error response bypassed framework envelope")
	}
	if status < 400 && output.Error != nil {
		t.Fatal("successful response has an error")
	}
	return output
}

func dataAs[T any](t *testing.T, output envelope) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(output.Data, &value); err != nil {
		t.Fatal("decode response DTO", err)
	}
	return value
}

func login(t *testing.T, engine *gin.Engine, username string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": username, "password": testPassword})
	output := request(t, engine, "POST", "/api/v1/auth/login", string(body), "", 200)
	return dataAs[struct {
		Token string `json:"token"`
	}](t, output).Token
}

func newRouter(t *testing.T, database *gorm.DB) *gin.Engine {
	t.Helper()
	ctx := context.Background()
	// First initialize the framework alone in a brand-new schema. Merely adding
	// an example package to the repository must never add its table or routes.
	if err := database.AutoMigrate(append(auth.Models(), settings.Models()...)...); err != nil {
		t.Fatal("initialize framework models")
	}
	if database.Migrator().HasTable("example_notes") {
		t.Fatal("default framework initialization created an example business table")
	}
	authentication, err := auth.New(database, configs.Auth{SessionTTL: time.Hour, MaxSessions: 5})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authentication.Close() })
	if err := authentication.Bootstrap(ctx, "admin", testPassword); err != nil {
		t.Fatal(err)
	}
	for name, permissions := range map[string][]string{
		"note_writer": {"example:notes:read", "example:notes:write"},
		"note_reader": {"example:notes:read"},
	} {
		if err := authentication.PutRole(ctx, name, permissions); err != nil {
			t.Fatal(err)
		}
	}
	for name, roles := range map[string][]string{
		"alice": {"note_writer"}, "bob": {"note_writer"}, "reader": {"note_reader"}, "blocked": {},
	} {
		if _, err := authentication.CreateUser(ctx, name, testPassword, roles); err != nil {
			t.Fatal(err)
		}
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	configuration, err := settings.New(database, base64.StdEncoding.EncodeToString(key), []settings.Definition{
		{Key: "system.display_name", Default: json.RawMessage(`"Note test"`)},
	})
	if err != nil || configuration.Initialize(ctx) != nil {
		t.Fatal("initialize framework settings")
	}
	resources := bootstrap.Resources{DB: database, Auth: authentication, Settings: configuration,
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	cfg := configs.Config{App: configs.App{Name: "note-http-test"}, HTTP: configs.HTTP{RequestTimeout: 10 * time.Second},
		Auth: configs.Auth{SessionTTL: time.Hour, LoginAttemptsPerMinute: 100}, Security: configs.Security{MaxBodyBytes: 128 * 1024}}
	container := &wire.Container{Config: cfg, Resources: resources,
		SystemController: controller.NewSystemController(authentication, configuration, int64(cfg.Auth.SessionTTL/time.Second))}
	engine, err := frameworkrouter.NewEngine(container)
	if err != nil {
		t.Fatal(err)
	}
	if err := frameworkrouter.New(engine, container); err != nil {
		t.Fatal(err)
	}
	request(t, engine, "GET", "/api/v1/examples/notes", "", "", 404)
	// The test registers only this example using the same explicit constructors
	// and route function calls as the application.
	if err := database.AutoMigrate(&model.Note{}); err != nil {
		t.Fatal("initialize opt-in note model")
	}
	store, err := mapper.NewNoteMapper(database)
	if err != nil {
		t.Fatal(err)
	}
	container.NoteController = controller.NewNoteController(impl.NewNoteService(store))
	protected := engine.Group("/api/v1", middleware.Authenticate(authentication))
	routes.RegisterNoteRoutes(container, protected)
	return engine
}

func TestNoteHTTPCRUDPermissionsOwnershipAndConflicts(t *testing.T) {
	engine := newRouter(t, testDatabase(t))
	const path = "/api/v1/examples/notes"
	request(t, engine, "GET", path, "", "", 401)
	request(t, engine, "POST", path, `{"slug":"first","title":"First"}`, "", 401)
	alice, bob, reader, blocked := login(t, engine, "alice"), login(t, engine, "bob"), login(t, engine, "reader"), login(t, engine, "blocked")
	request(t, engine, "GET", path, "", blocked, 403)
	request(t, engine, "POST", path, `{"slug":"first","title":"First"}`, blocked, 403)
	request(t, engine, "GET", path, "", reader, 200)
	request(t, engine, "POST", path, `{"slug":"first","title":"First"}`, reader, 403)
	created := request(t, engine, "POST", path, `{"slug":"first-note","title":"First","content":"Original"}`, alice, 201)
	item := dataAs[vo.NoteVO](t, created)
	if item.ID == 0 || item.Revision != 1 || item.Slug != "first-note" {
		t.Fatal("create DTO missing ID, slug or initial revision")
	}
	if !strings.Contains(string(created.Data), `"id":"`) || strings.Contains(string(created.Data), "owner") {
		t.Fatal("model fields leaked or ID lost string representation")
	}
	one := path + "/" + strconv.FormatUint(item.ID, 10)
	request(t, engine, "GET", one, "", alice, 200)
	duplicate := request(t, engine, "POST", path, `{"slug":"first-note","title":"Duplicate"}`, alice, 409)
	if duplicate.Error.Code != "example_note_conflict" {
		t.Fatal("duplicate slug error lost stable business code")
	}
	// The same slug is legal for another owner; ID access remains owner scoped.
	request(t, engine, "POST", path, `{"slug":"first-note","title":"Bob"}`, bob, 201)
	request(t, engine, "GET", one, "", bob, 404)
	request(t, engine, "GET", one, "", reader, 404)
	request(t, engine, "PUT", one, `{"title":"Hijack","content":"x","revision":1}`, bob, 404)
	request(t, engine, "DELETE", one+"?revision=1", "", bob, 404)
	list := dataAs[vo.NotePageVO](t, request(t, engine, "GET", path+"?page=1&pageSize=1", "", alice, 200))
	if list.Total != 1 || len(list.Items) != 1 || list.Page != 1 || list.PageSize != 1 || list.Items[0].Title != "First" {
		t.Fatal("pagination exposed another owner's note or wrong metadata")
	}
	second := dataAs[vo.NotePageVO](t, request(t, engine, "GET", path+"?page=2&pageSize=1", "", alice, 200))
	if second.Total != 1 || len(second.Items) != 0 || second.Items == nil {
		t.Fatal("empty page contract regressed")
	}
	updated := dataAs[vo.NoteVO](t, request(t, engine, "PUT", one, `{"title":"Edited","content":"","revision":1}`, alice, 200))
	if updated.Revision != 2 || updated.Content != "" || updated.Slug != "first-note" {
		t.Fatal("full update did not preserve slug and increment revision")
	}
	request(t, engine, "PUT", one, `{"title":"Stale","content":"x","revision":1}`, alice, 409)
	request(t, engine, "DELETE", one+"?revision=1", "", alice, 409)
	deleted := request(t, engine, "DELETE", one+"?revision=2", "", alice, 200)
	if !dataAs[struct {
		Deleted bool `json:"deleted"`
	}](t, deleted).Deleted {
		t.Fatal("delete response contract missing")
	}
	missing := request(t, engine, "GET", one, "", alice, 404)
	if missing.Error.Code != "example_note_not_found" {
		t.Fatal("not-found error lost stable business code")
	}
	request(t, engine, "DELETE", one+"?revision=2", "", alice, 404)
}

func TestNoteHTTPRejectsMassAssignmentAndMalformedParameters(t *testing.T) {
	engine := newRouter(t, testDatabase(t))
	const path = "/api/v1/examples/notes"
	token := login(t, engine, "alice")
	for _, body := range []string{
		`{"slug":"ok","title":"Title","owner_id":1}`,
		`{"slug":"ok","title":"Title","revision":100}`,
		`{"slug":"INVALID","title":"Title"}`,
		`{"slug":"ok","title":"   "}`,
		`{"slug":"ok"}`,
	} {
		request(t, engine, "POST", path, body, token, 400)
	}
	item := dataAs[vo.NoteVO](t, request(t, engine, "POST", path, `{"slug":"editable","title":"Title"}`, token, 201))
	one := path + "/" + strconv.FormatUint(item.ID, 10)
	for _, body := range []string{
		`{"title":"Edit","revision":1}`,
		`{"content":"Edit","revision":1}`,
		`{"title":"Edit","content":"x"}`,
		`{"title":null,"content":"x","revision":1}`,
		`{"title":"Edit","content":"x","revision":0}`,
		`{"title":"Edit","content":"x","revision":1,"slug":"change"}`,
	} {
		request(t, engine, "PUT", one, body, token, 400)
	}
	for _, query := range []string{
		"?page=0", "?page=-1", "?page=01", "?page=10001", "?page=abc", "?page=", "?page=1&page=2",
		"?pageSize=101", "?pageSize=0", "?page_size=2", "?page=%ZZ", "?page=1;foo=2",
	} {
		request(t, engine, "GET", path+query, "", token, 400)
	}
	for _, query := range []string{"", "?revision=0", "?revision=-1", "?revision=01", "?revision=x", "?revision=1&revision=2", "?revision=1&owner_id=1", "?revision=%ZZ", "?revision=1;foo=2"} {
		request(t, engine, "DELETE", one+query, "", token, 400)
	}
	for _, id := range []string{"0", "-1", "01", "abc", "9223372036854775808"} {
		request(t, engine, "GET", path+"/"+id, "", token, 400)
	}
	list := dataAs[vo.NotePageVO](t, request(t, engine, "GET", path, "", token, 200))
	if list.Total != 1 || list.Items[0].Revision != 1 || list.PageSize != 20 {
		t.Fatal("rejected requests mutated state or defaults changed")
	}
}

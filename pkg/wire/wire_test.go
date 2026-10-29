package wire

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"heago-scaffold/configs"
	"heago-scaffold/internal/db"
	"heago-scaffold/internal/testutil"
	"heago-scaffold/pkg/bootstrap"

	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// wireDatabase 为手写和生成两种注入路径准备真实隔离数据库
// 测试先完成对象构造，再选择 schema，最后执行公共初始化收尾
// One pool connection keeps SET search_path attached to every initialization I/O.
func wireDatabase(t *testing.T) (configs.Config, *gorm.DB, string) {
	t.Helper()
	dsn := os.Getenv("HEAGO_TEST_DATABASE_URL")
	if dsn == "" {
		return wireMySQLDatabase(t)
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.User == nil || u.Hostname() == "" {
		t.Fatal("Wire integration tests require a PostgreSQL URL")
	}
	port := 5432
	if u.Port() != "" {
		port, err = strconv.Atoi(u.Port())
		if err != nil {
			t.Fatal("invalid test database port")
		}
	}
	password, _ := u.User.Password()
	sslMode := u.Query().Get("sslmode")
	if sslMode == "" {
		sslMode = "require"
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal("generate isolated Wire schema")
	}
	schema := "wire_test_" + hex.EncodeToString(random[:])
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("open test PostgreSQL")
	}
	adminPool, err := admin.DB()
	if err != nil {
		t.Fatal("obtain test PostgreSQL pool")
	}
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		_ = adminPool.Close()
		t.Fatal("create isolated Wire schema")
	}
	t.Cleanup(func() {
		if err := admin.Exec("DROP SCHEMA " + schema + " CASCADE").Error; err != nil {
			t.Error("drop isolated Wire schema")
		}
		_ = adminPool.Close()
	})
	cfg := wireConfig(t, configs.Database{Host: u.Hostname(), Port: port, Name: strings.TrimPrefix(u.Path, "/"),
		User: u.User.Username(), Password: password, SSLMode: sslMode, MaxOpen: 1, MaxIdle: 1,
		ConnMaxLifetime: time.Hour, ConnMaxIdleTime: 30 * time.Minute, ConnectTimeout: 5 * time.Second,
		RetryAttempts: 1, RetryDelay: time.Millisecond})
	return cfg, admin, schema
}

// MySQL isolates tests with separate databases rather than search_path. The
// returned config lets both real injection graphs open their own bounded pool.
func wireMySQLDatabase(t *testing.T) (configs.Config, *gorm.DB, string) {
	t.Helper()
	dsn := os.Getenv("HEAGO_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set HEAGO_TEST_DATABASE_URL or HEAGO_TEST_MYSQL_DSN for real Wire integration tests")
	}
	connection, err := mysqldriver.ParseDSN(dsn)
	if err != nil || connection.Net != "tcp" {
		t.Fatal("Wire MySQL tests require a TCP DSN")
	}
	host, portText, err := net.SplitHostPort(connection.Addr)
	if err != nil {
		t.Fatal("parse MySQL test address")
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal("parse MySQL test port")
	}
	database := testutil.OpenDatabase(t)
	var name string
	if err := database.Raw("SELECT DATABASE()").Scan(&name).Error; err != nil || name == "" {
		t.Fatal("obtain isolated MySQL database name")
	}
	tlsMode := connection.TLSConfig
	if tlsMode == "" {
		tlsMode = "false"
	}
	cfg := wireConfig(t, configs.Database{Driver: "mysql", Host: host, Port: port, Name: name,
		User: connection.User, Password: connection.Passwd, TLS: tlsMode, MaxOpen: 1, MaxIdle: 1,
		ConnMaxLifetime: time.Hour, ConnMaxIdleTime: 30 * time.Minute, ConnectTimeout: 5 * time.Second,
		RetryAttempts: 1, RetryDelay: time.Millisecond})
	return cfg, database, ""
}

func wireConfig(t *testing.T, database configs.Database) configs.Config {
	t.Helper()
	return configs.Config{
		App: configs.App{Name: "wire-integration", Environment: "test"},
		HTTP: configs.HTTP{Addr: ":18089", ReadHeaderTimeout: time.Second, ReadTimeout: 5 * time.Second,
			WriteTimeout: 10 * time.Second, IdleTimeout: time.Minute, ShutdownTimeout: time.Second,
			RequestTimeout: 5 * time.Second},
		Database:  database,
		Auth:      configs.Auth{SessionTTL: time.Hour, MaxSessions: 5, LoginAttemptsPerMinute: 60},
		Bootstrap: configs.Bootstrap{AdminUsername: "admin", AdminPassword: "wire-admin-password-7b91"},
		Security:  configs.Security{EncryptionKey: base64.StdEncoding.EncodeToString(randomKey(t)), MaxBodyBytes: 1024},
		Logging:   configs.Logging{Level: "error", Format: "json"},
	}
}

func randomKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal("generate test encryption key")
	}
	return key
}

// 单连接测试池的 search_path 只作用于本次构造的资源，不修改默认 schema
// 先执行真实构造图，再指定隔离 schema，最后走公共入口相同的 finish
type graphBuilder func(context.Context, configs.Config, bootstrap.Options) (*Container, func(), error)

func isolatedGraph(t *testing.T, build graphBuilder, cfg configs.Config, schema string, opts bootstrap.Options) (*Container, error) {
	t.Helper()
	c, cleanup, err := build(context.Background(), cfg, opts)
	if err != nil {
		return nil, err
	}
	if schema != "" {
		if err := c.DB.Exec("SET search_path TO " + schema).Error; err != nil {
			cleanup()
			t.Fatal("set isolated injection schema")
		}
	}
	database := c.DB
	result, err := finish(context.Background(), c, cleanup, opts)
	if err != nil {
		assertPoolClosed(t, database)
	}
	return result, err
}

func assertPoolClosed(t *testing.T, database *gorm.DB) {
	t.Helper()
	pool, err := database.DB()
	if err != nil {
		t.Fatal("obtain captured database pool")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := pool.PingContext(ctx); err == nil {
		t.Fatal("cleanup left database pool open")
	}
}

func TestManualAndGeneratedGraphsPreserveSeedsAndExampleIsolation(t *testing.T) {
	for name, build := range map[string]graphBuilder{"manual": buildManual, "wire": buildWithWire} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			cfg, _, schema := wireDatabase(t)
			opts := bootstrap.Options{InitializeSchema: true}
			c, err := isolatedGraph(t, build, cfg, schema, opts)
			if err != nil {
				t.Fatal("initialize dependency graph", err)
			}
			t.Cleanup(c.Close)
			if c.SystemController == nil || c.NoteController == nil || c.RealtimeController == nil || c.NoteService == nil {
				t.Fatal("dependency graph is incomplete")
			}
			if c.EnableExamples || c.DB.Migrator().HasTable("example_notes") {
				t.Fatal("default graph created example tables")
			}
			if _, _, err := c.Auth.Login(ctx, "admin", cfg.Bootstrap.AdminPassword); err != nil {
				t.Fatal("admin seed missing")
			}
			view, err := c.Settings.Update(ctx, "system.display_name", json.RawMessage(`"Saved through injection"`), 1, "1")
			if err != nil || view.Revision != 2 {
				t.Fatal("settings write failed")
			}
			c.Close()
			assertPoolClosed(t, c.DB)

			// 再次构造不会重置密码，也不会用文件默认值覆盖数据库配置
			cfg.Bootstrap.AdminPassword = "different-bootstrap-password-7b91"
			opts.InitializeSchema = false
			restarted, err := isolatedGraph(t, build, cfg, schema, opts)
			if err != nil {
				t.Fatal("restart dependency graph", err)
			}
			t.Cleanup(restarted.Close)
			if _, _, err := restarted.Auth.Login(ctx, "admin", "wire-admin-password-7b91"); err != nil {
				t.Fatal("restart replaced administrator password")
			}
			saved, err := restarted.Settings.Get(ctx, "system.display_name")
			if err != nil || string(saved.Value) != `"Saved through injection"` || saved.Revision != 2 {
				t.Fatal("restart replaced persisted setting")
			}
			restarted.Close()

			opts.InitializeSchema, opts.EnableExamples = true, true
			example, err := isolatedGraph(t, build, cfg, schema, opts)
			if err != nil {
				t.Fatal("initialize example graph", err)
			}
			t.Cleanup(example.Close)
			if !example.EnableExamples || !example.DB.Migrator().HasTable("example_notes") {
				t.Fatal("example model not registered")
			}
		})
	}
}

func TestPreparationFailureClosesBothGraphs(t *testing.T) {
	for name, build := range map[string]graphBuilder{"manual": buildManual, "wire": buildWithWire} {
		t.Run(name, func(t *testing.T) {
			cfg, _, schema := wireDatabase(t)
			c, err := isolatedGraph(t, build, cfg, schema, bootstrap.Options{})
			if c != nil || !errors.Is(err, db.ErrSchema) {
				t.Fatal("normal startup silently migrated absent tables", err)
			}
		})
	}
}

func TestContainerAndRuntimeCleanupAreIdempotent(t *testing.T) {
	var calls atomic.Int32
	c := &Container{cleanup: func() { calls.Add(1) }}
	runtime := bootstrap.NewRuntime(bootstrap.Components{}, c.Close)
	var group sync.WaitGroup
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func() { defer group.Done(); runtime.Close(); c.Close() }()
	}
	group.Wait()
	runtime.Close()
	if calls.Load() != 1 {
		t.Fatal("cleanup must run exactly once")
	}
}

func TestInvalidGraphInputs(t *testing.T) {
	for name, constructor := range map[string]func(context.Context, configs.Config, bootstrap.Options) (*Container, error){"manual": NewContainer, "wire": NewContainerWithWire} {
		t.Run(name, func(t *testing.T) {
			if c, err := constructor(context.Background(), configs.Config{}, bootstrap.Options{}); err == nil || c != nil {
				t.Fatal("invalid config accepted")
			}
			if c, err := constructor(nil, configs.Config{}, bootstrap.Options{}); err == nil || c != nil {
				t.Fatal("nil context accepted")
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if c, err := constructor(ctx, configs.Config{}, bootstrap.Options{}); !errors.Is(err, context.Canceled) || c != nil {
				t.Fatal("canceled context accepted")
			}
		})
	}
}

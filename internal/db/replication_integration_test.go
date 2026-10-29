package db_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"healgo-scaffold/configs"
	"healgo-scaffold/internal/auth"
	database "healgo-scaffold/internal/db"
	"healgo-scaffold/internal/middleware"
	model "healgo-scaffold/internal/model/note"
	"healgo-scaffold/internal/settings"
	"healgo-scaffold/pkg/serve/controller"
	"healgo-scaffold/pkg/serve/mapper"
	"healgo-scaffold/pkg/serve/service/impl"

	"github.com/gin-gonic/gin"
	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// This suite controls only the disposable, labelled replication harness. Opting
// in is mandatory because pausing/stopping a replica is a deliberate mutation.
// Drivers run sequentially; parallel fault injection would conceal node state.
func TestReplicationIntegration(t *testing.T) {
	if os.Getenv("HEAGO_TEST_REPLICATION") != "1" {
		t.Skip("set HEAGO_TEST_REPLICATION=1 and start the disposable replication harness")
	}
	previousMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	defer gin.SetMode(previousMode)
	for _, engine := range []string{"Postgres", "MySQL"} {
		t.Run(engine, func(t *testing.T) { testReplicationEngine(t, engine) })
	}
}

func testReplicationEngine(t *testing.T, engine string) {
	t.Helper()
	cfg := replicationConfig(t, engine)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cluster, err := database.OpenCluster(ctx, cfg, log)
	if err != nil {
		t.Fatal("open primary/replica cluster")
	}
	defer cluster.Close()
	strictCfg := cfg
	strictCfg.FallbackToPrimary = false
	strict, err := database.OpenCluster(ctx, strictCfg, log)
	if err != nil {
		t.Fatal("open cluster with fallback disabled")
	}
	defer strict.Close()
	var replicaHandle *gorm.DB
	if err := cluster.Read(ctx, func(db *gorm.DB) error { replicaHandle = db; return nil }); err != nil {
		t.Fatal("capture replica pool")
	}
	models := append(auth.Models(), settings.Models()...)
	models = append(models, &model.Note{})
	if err := database.Initialize(ctx, cluster.Primary, models); err != nil {
		t.Fatal("initialize replication test tables")
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal("generate replication test identity")
	}
	suffix := hex.EncodeToString(random[:])
	username, settingKey := "repl-"+suffix, "replication.test_"+suffix
	var owner uint64
	paused, stopped := false, false
	// Restore node state even after a failed assertion. A separate cleanup pool
	// is necessary because the assertions below explicitly close both clusters.
	t.Cleanup(func() {
		if stopped {
			if err := controlReplication("StartReplica", engine); err != nil {
				t.Error("restore stopped replica failed")
			}
		}
		if paused {
			if err := controlReplication("Resume", engine); err != nil {
				t.Error("resume paused replica failed")
			}
		}
		cleanupCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		cleanup, err := database.Open(cleanupCtx, cfg, log)
		if err != nil {
			t.Error("open replication data cleanup connection")
			return
		}
		defer database.Close(cleanup)
		// Every predicate belongs to this test. Existing users, settings and the
		// harness probe are retained; no database or shared table is dropped.
		for _, operation := range []func() error{
			func() error {
				if owner == 0 {
					return nil
				}
				return cleanup.WithContext(cleanupCtx).Where("owner_id = ?", owner).Delete(&model.Note{}).Error
			},
			func() error {
				return cleanup.WithContext(cleanupCtx).Where(clause.Eq{Column: "key", Value: settingKey}).Delete(&settings.Audit{}).Error
			},
			func() error {
				return cleanup.WithContext(cleanupCtx).Table("sys_settings").Where(clause.Eq{Column: "key", Value: settingKey}).Delete(nil).Error
			},
			func() error {
				return cleanup.WithContext(cleanupCtx).Where("username = ?", username).Delete(&auth.User{}).Error
			},
		} {
			if err := operation(); err != nil {
				t.Error("remove owned replication test data")
			}
		}
	})
	authentication, err := auth.New(cluster.Primary, configs.Auth{SessionTTL: time.Hour, MaxSessions: 5})
	if err != nil {
		t.Fatal("construct authentication service")
	}
	defer authentication.Close()
	principal, err := authentication.CreateUser(ctx, username, "replication-password-1", nil)
	if err != nil {
		t.Fatal("create replication test identity")
	}
	owner = principal.ID
	token, _, err := authentication.Login(ctx, username, "replication-password-1")
	if err != nil {
		t.Fatal("login replication test identity")
	}
	settingService, err := settings.New(cluster.Primary, base64.StdEncoding.EncodeToString(make([]byte, 32)),
		[]settings.Definition{{Key: settingKey, Default: json.RawMessage(`"before"`)}})
	if err != nil || settingService.Initialize(ctx) != nil {
		t.Fatal("initialize isolated runtime setting")
	}
	writer, err := mapper.NewNoteMapper(cluster.Primary)
	if err != nil {
		t.Fatal("construct writer mapper")
	}
	reader, err := mapper.NewNoteMapperWithReader(cluster)
	if err != nil {
		t.Fatal("construct replica list mapper")
	}
	strictReader, err := mapper.NewNoteMapperWithReader(strict)
	if err != nil {
		t.Fatal("construct strict replica list mapper")
	}
	if _, err := writer.Create(ctx, model.Note{OwnerID: owner, Slug: "before-" + suffix, Title: "Before pause"}); err != nil {
		t.Fatal("write replication baseline")
	}
	waitForReplicaNotes(t, ctx, strictReader, "baseline", owner, 1)
	paused = true
	if err := controlReplication("Pause", engine); err != nil {
		t.Fatal("pause replication apply")
	}
	created, err := writer.Create(ctx, model.Note{OwnerID: owner, Slug: "after-" + suffix, Title: "After pause"})
	if err != nil {
		t.Fatal("write while replica is paused")
	}
	if actual, err := writer.Get(ctx, owner, created.ID); err != nil || actual.ID != created.ID {
		t.Fatal("primary detail did not see committed write immediately")
	}
	beforeLagRead := cluster.Stats()
	items, total, err := reader.List(ctx, owner, 0, 20)
	if err != nil || total != 1 || len(items) != 1 || items[0].ID == created.ID {
		t.Fatal("paused replica unexpectedly saw a new primary write")
	}
	if cluster.Stats().PrimaryFallbacks != beforeLagRead.PrimaryFallbacks {
		t.Fatal("ordinary replica lag incorrectly triggered primary fallback")
	}
	// The latest revision and conflict classification must stay on the writer.
	// Exercise the actual controller so a stale client sees HTTP 409, not 404.
	if updated, err := writer.Update(ctx, owner, created.ID, 1, "Updated", ""); err != nil || updated.Revision != 2 {
		t.Fatal("update primary revision while replica is paused")
	}
	notes := controller.NewNoteController(impl.NewNoteService(reader))
	engineRouter := gin.New()
	engineRouter.PUT("/notes/:id", middleware.Authenticate(authentication), notes.Update)
	request := httptest.NewRequest(http.MethodPut, "/notes/"+strconv.FormatUint(created.ID, 10), strings.NewReader(`{"title":"Stale edit","content":"","revision":1}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	engineRouter.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("stale revision status=%d; want 409", response.Code)
	}
	if err := authentication.ChangePassword(ctx, owner, "replication-password-1", "replication-password-2"); err != nil {
		t.Fatal("change password on primary")
	}
	request = httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	if _, _, err := authentication.Authenticate(ctx, request); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatal("paused replica allowed a revoked credential")
	}
	if view, err := settingService.Update(ctx, settingKey, json.RawMessage(`"after"`), 1, username); err != nil || view.Revision != 2 {
		t.Fatal("save setting to primary")
	}
	if view, err := settingService.Get(ctx, settingKey); err != nil || view.Revision != 2 || string(view.Value) != `"after"` {
		t.Fatal("settings read did not observe committed primary revision")
	}
	if _, err := settingService.Update(ctx, settingKey, json.RawMessage(`"stale"`), 1, username); !errors.Is(err, settings.ErrConflict) {
		t.Fatal("settings stale revision was not a primary conflict")
	}
	var replicaRevision uint64
	if err := cluster.Read(ctx, func(db *gorm.DB) error {
		return db.Table("sys_settings").Where(clause.Eq{Column: "key", Value: settingKey}).Pluck("revision", &replicaRevision).Error
	}); err != nil || replicaRevision != 1 {
		t.Fatal("replica setting changed despite paused replication")
	}
	assertReplicaErrorsDoNotFallback(t, ctx, cluster, owner)
	// A separate reader connection targets the primary with the reader account.
	// This proves SQL grants reject writes independently of replica read-only mode.
	readOnlyConfig := cfg.ReplicaConfigs()[0]
	readOnlyConfig.Host, readOnlyConfig.Port = cfg.PrimaryConfig().Host, cfg.PrimaryConfig().Port
	readOnly, err := database.Open(ctx, readOnlyConfig, log)
	if err != nil {
		t.Fatal("open reader account for write-denial check")
	}
	defer database.Close(readOnly)
	if err := readOnly.WithContext(ctx).Model(&model.Note{}).Where("owner_id = ?", owner).Update("title", "Forbidden write").Error; err == nil {
		t.Fatal("reader database account unexpectedly accepted a write")
	}
	if err := controlReplication("Resume", engine); err != nil {
		t.Fatal("resume replication")
	}
	paused = false
	waitForReplicaNotes(t, ctx, strictReader, "resume", owner, 2)
	stopped = true
	if err := controlReplication("StopReplica", engine); err != nil {
		t.Fatal("stop disposable replica")
	}
	beforeFallback := cluster.Stats()
	if items, total, err := reader.List(ctx, owner, 0, 20); err != nil || total != 2 || len(items) != 2 {
		t.Fatal("failed replica did not return a complete primary page")
	}
	afterFallback := cluster.Stats()
	if afterFallback.PrimaryFallbacks != beforeFallback.PrimaryFallbacks+1 || afterFallback.ReplicaAttempts != beforeFallback.ReplicaAttempts+1 {
		t.Fatal("one failed replica page did not record exactly one fallback")
	}
	strictCalls := 0
	if err := strict.Read(ctx, func(db *gorm.DB) error {
		strictCalls++
		var count int64
		return db.Model(&model.Note{}).Where("owner_id = ?", owner).Count(&count).Error
	}); err == nil || strictCalls != 1 || strict.Stats().PrimaryFallbacks != 0 {
		t.Fatal("fallback-disabled cluster silently read the primary")
	}
	if err := database.Ping(ctx, cluster.Primary); err != nil {
		t.Fatal("replica failure incorrectly made primary readiness fail")
	}
	if err := controlReplication("StartReplica", engine); err != nil {
		t.Fatal("restart disposable replica")
	}
	stopped = false
	waitForReplicaNotes(t, ctx, strictReader, "restart", owner, 2)
	primaryHandle := cluster.Primary
	if cluster.Close() != nil || cluster.Close() != nil || strict.Close() != nil || strict.Close() != nil {
		t.Fatal("cluster close was not successful and idempotent")
	}
	for _, handle := range []*gorm.DB{primaryHandle, replicaHandle, strict.Primary} {
		pool, err := handle.DB()
		if err != nil {
			t.Fatal("obtain captured pool after close")
		}
		if pool.PingContext(ctx) == nil {
			t.Fatal("cluster close leaked a primary or replica connection pool")
		}
	}
}

func assertReplicaErrorsDoNotFallback(t *testing.T, ctx context.Context, cluster *database.Cluster, owner uint64) {
	t.Helper()
	for name, query := range map[string]func(*gorm.DB) error{
		"syntax": func(db *gorm.DB) error { var value int; return db.Raw("SELECT FROM").Scan(&value).Error },
		"not found": func(db *gorm.DB) error {
			var row model.Note
			return db.Where("owner_id = ? AND id = ?", owner, uint64(9223372036854775807)).First(&row).Error
		},
	} {
		before, calls := cluster.Stats(), 0
		err := cluster.Read(ctx, func(db *gorm.DB) error { calls++; return query(db) })
		if err == nil || calls != 1 || cluster.Stats().PrimaryFallbacks != before.PrimaryFallbacks {
			t.Fatalf("%s incorrectly triggered primary fallback", name)
		}
		if name == "not found" && !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatal("missing replica row did not preserve record-not-found classification")
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	before, called := cluster.Stats(), false
	if err := cluster.Read(canceled, func(*gorm.DB) error { called = true; return nil }); !errors.Is(err, context.Canceled) || called || cluster.Stats() != before {
		t.Fatal("already canceled parent context performed database I/O")
	}
	duringRead, stop := context.WithCancel(ctx)
	defer stop()
	if err := cluster.Read(duringRead, func(*gorm.DB) error { stop(); return duringRead.Err() }); !errors.Is(err, context.Canceled) || cluster.Stats().PrimaryFallbacks != before.PrimaryFallbacks {
		t.Fatal("canceled parent context triggered primary fallback")
	}
}

func waitForReplicaNotes(t *testing.T, ctx context.Context, notes *mapper.NoteMapper, phase string, owner uint64, expected int64) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		items, total, err := notes.List(ctx, owner, 0, 20)
		if err == nil && total == expected && int64(len(items)) == expected {
			return
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			// Mapper errors deliberately hide SQL/connection details. Fixed labels
			// still distinguish lag from a broken query without exposing credentials.
			t.Fatalf("replica catch-up phase=%s last_error=%s observed_total=%d observed_items=%d expected=%d parent_context=%s",
				phase, replicationErrorCategory(err), total, len(items), expected, replicationErrorCategory(ctx.Err()))
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			t.Fatalf("replica catch-up phase=%s last_error=%s observed_total=%d observed_items=%d expected=%d parent_context=%s",
				phase, replicationErrorCategory(err), total, len(items), expected, replicationErrorCategory(ctx.Err()))
		}
	}
}

func replicationErrorCategory(err error) string {
	// Never format err itself: a driver or wrapped error may contain SQL values,
	// addresses, or credentials. Only explicit application/context categories leave.
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "context_deadline"
	case errors.Is(err, model.ErrInvalidInput):
		return "invalid_input"
	case errors.Is(err, model.ErrNotFound), errors.Is(err, gorm.ErrRecordNotFound):
		return "not_found"
	case errors.Is(err, model.ErrUnavailable), errors.Is(err, database.ErrUnavailable):
		return "storage_unavailable"
	default:
		return "unclassified"
	}
}

func controlReplication(action, engine string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "pwsh", "-NoProfile", "-File", filepath.Join("..", "..", "scripts", "replication", "replication.ps1"), "-Action", action, "-Database", engine)
	// Harness output is intentionally discarded; test failures name the action
	// without exposing any unexpected diagnostic containing local credentials.
	if err := command.Run(); err != nil {
		return errors.New("replication harness operation failed")
	}
	return nil
}

func replicationConfig(t *testing.T, engine string) configs.Database {
	t.Helper()
	prefix := "HEAGO_TEST_" + strings.ToUpper(engine)
	primaryDSN, replicaDSN := os.Getenv(prefix+"_PRIMARY_DSN"), os.Getenv(prefix+"_REPLICA_DSN")
	if primaryDSN == "" || replicaDSN == "" {
		t.Fatal("both replication primary and replica DSNs are required")
	}
	connection := func(dsn string) configs.Endpoint {
		if engine == "Postgres" {
			u, err := url.Parse(dsn)
			if err != nil || u.User == nil || u.Hostname() == "" || u.Scheme != "postgres" {
				t.Fatal("invalid replication PostgreSQL URL")
			}
			port := 5432
			if u.Port() != "" {
				port, err = strconv.Atoi(u.Port())
				if err != nil {
					t.Fatal("invalid replication PostgreSQL port")
				}
			}
			password, _ := u.User.Password()
			return configs.Endpoint{Host: u.Hostname(), Port: port, Name: strings.TrimPrefix(u.Path, "/"), User: u.User.Username(), Password: password, SSLMode: u.Query().Get("sslmode")}
		}
		parsed, err := mysqldriver.ParseDSN(dsn)
		if err != nil || parsed.Net != "tcp" {
			t.Fatal("invalid replication MySQL TCP DSN")
		}
		host, portText, err := net.SplitHostPort(parsed.Addr)
		if err != nil {
			t.Fatal("invalid replication MySQL address")
		}
		port, err := strconv.Atoi(portText)
		if err != nil {
			t.Fatal("invalid replication MySQL port")
		}
		tls := parsed.TLSConfig
		if tls == "" {
			tls = "false"
		}
		return configs.Endpoint{Host: host, Port: port, Name: parsed.DBName, User: parsed.User, Password: parsed.Passwd, TLS: tls}
	}
	primary, replica := connection(primaryDSN), connection(replicaDSN)
	driver := "postgres"
	if engine == "MySQL" {
		driver = "mysql"
	}
	return configs.Database{Driver: driver, Primary: &primary, Replicas: []configs.Endpoint{replica},
		FallbackToPrimary: true, MaxOpen: 4, MaxIdle: 2, ConnMaxLifetime: time.Minute,
		ConnMaxIdleTime: 30 * time.Second, ConnectTimeout: time.Second, RetryAttempts: 1, RetryDelay: time.Millisecond}
}

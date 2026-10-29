package db

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"healgo-scaffold/configs"
	"healgo-scaffold/internal/testutil"

	mysqldriver "github.com/go-sql-driver/mysql"
	gormlogger "gorm.io/gorm/logger"
)

func TestConnectionURLEscapesCredentialValues(t *testing.T) {
	c := configs.Database{Host: "::1", Port: 5432, Name: "template db", User: "user@host", Password: "p @:/?#=\\value", SSLMode: "disable", ConnectTimeout: 1500 * time.Millisecond}
	u, err := url.Parse(connectionURL(c))
	if err != nil {
		t.Fatal(err)
	}
	password, _ := u.User.Password()
	if password != c.Password || u.User.Username() != c.User || u.Host != net.JoinHostPort(c.Host, "5432") || u.Path != "/"+c.Name || u.Query().Get("connect_timeout") != "2" {
		t.Fatal("connection values did not round trip")
	}
	if len(u.Query()) != 2 {
		t.Fatal("credential value injected a connection option")
	}
}

// 密码里的 @:/? 与查询选项字符必须只被当成密码，不能改变连接目标或TLS
func TestMySQLConnectionValuesRoundTrip(t *testing.T) {
	c := configs.Database{Driver: "mysql", Host: "::1", Port: 3306, Name: "template db", User: "heago", Password: "p @:/?#=\\value", TLS: "false", ConnectTimeout: time.Second}
	parsed, err := mysqldriver.ParseDSN(mysqlDSN(c))
	if err != nil {
		t.Fatal("parse structured MySQL DSN")
	}
	if parsed.User != c.User || parsed.Passwd != c.Password || parsed.Addr != "[::1]:3306" || parsed.DBName != c.Name || parsed.TLSConfig != "false" || !parsed.ParseTime || parsed.Loc != time.UTC {
		t.Fatal("MySQL connection values did not round trip")
	}
	// 驱动将 charset 收入内部字段；额外Params应为空，已知选项也保持原值
	if len(parsed.Params) != 0 || parsed.Collation != "utf8mb4_bin" || parsed.Timeout != time.Second || parsed.InterpolateParams {
		t.Fatal("password injected connection options")
	}
}

func TestOpenRespectsCancellationDuringRetry(t *testing.T) {
	// An already closed local port fails quickly; a long retry timer must not delay cancellation.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	cfg := configs.Database{Host: "127.0.0.1", Port: port, Name: "unused", User: "unused", Password: "private-value", SSLMode: "disable", MaxOpen: 1, MaxIdle: 0, ConnectTimeout: time.Second, RetryDelay: 30 * time.Second, RetryAttempts: 10}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = Open(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wanted context cancellation, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("retry backoff ignored cancellation")
	}
	if strings.Contains(err.Error(), cfg.Password) {
		t.Fatal("connection error exposed credentials")
	}
}

func TestDatabaseLoggerNeverEvaluatesOrPrintsSQL(t *testing.T) {
	var output bytes.Buffer
	l := safeLogger{log: slog.New(slog.NewJSONHandler(&output, nil)), level: gormlogger.Info}
	secret := "a-password-that-must-remain-private"
	l.Trace(context.Background(), time.Now(), func() (string, int64) { t.Fatal("SQL renderer was evaluated"); return "", 0 }, errors.New(secret))
	l.Error(context.Background(), secret, secret)
	l.Warn(context.Background(), secret, secret)
	if strings.Contains(output.String(), secret) || strings.Contains(output.String(), "SELECT") {
		t.Fatal("database logger leaked SQL or driver errors")
	}
}

type schemaProbe struct {
	ID    uint `gorm:"primaryKey"`
	Value string
}

func (schemaProbe) TableName() string { return "schema_probes" }

func TestInitializeAndSchemaCheck(t *testing.T) {
	database := testutil.OpenDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	models := []any{&schemaProbe{}}
	if err := CheckSchema(ctx, database, models); !errors.Is(err, ErrSchema) {
		t.Fatalf("missing schema was accepted: %v", err)
	}
	if err := Initialize(ctx, database, models); err != nil {
		t.Fatal(err)
	}
	if err := Initialize(ctx, database, models); err != nil {
		t.Fatal("initialization must be repeatable")
	}
	if err := CheckSchema(ctx, database, models); err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&schemaProbe{Value: "retained"}).Error; err != nil {
		t.Fatal("cannot insert probe")
	}
	if err := Initialize(ctx, database, models); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := database.Model(&schemaProbe{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatal("initialization changed existing rows")
	}
	if err := Ping(ctx, database); err != nil {
		t.Fatal(err)
	}
	if err := Close(database); err != nil {
		t.Fatal(err)
	}
	if err := Ping(ctx, database); !errors.Is(err, ErrUnavailable) {
		t.Fatal(fmt.Sprintf("closed connection returned %v", err))
	}
}

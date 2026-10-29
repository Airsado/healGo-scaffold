// Package db owns database connectivity and the explicit GORM schema entry point.
// Services receive the connection through constructors; no package-global database is used.
package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/url"
	"strconv"
	"time"

	"heago-scaffold/configs"

	mysqldriver "github.com/go-sql-driver/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

var (
	// Exported sentinel errors let readiness checks classify failure without parsing driver text.
	// Detailed connection values stay in the private deployment file, rather than log output.
	ErrUnavailable = errors.New("database is unavailable; check connection configuration and server status")
	ErrSchema      = errors.New("database schema is unavailable; initialize the schema before serving requests")
)

// Open retries only a bounded number of times and lets shutdown cancel both a dial
// and its backoff timer. Driver errors are never returned because they may include a DSN.
func Open(ctx context.Context, cfg configs.Database, log *slog.Logger) (*gorm.DB, error) {
	// Reject cancellation before constructing any connection pool or doing DNS work.
	if ctx == nil {
		return nil, errors.New("database context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg = cfg.PrimaryConfig()
	if cfg.EffectiveDriver() != "postgres" && cfg.EffectiveDriver() != "mysql" {
		return nil, errors.New("unsupported database driver")
	}
	if cfg.RetryAttempts < 1 || cfg.RetryAttempts > 10 || cfg.ConnectTimeout <= 0 || cfg.RetryDelay <= 0 || cfg.MaxOpen < 1 || cfg.MaxIdle < 0 || cfg.MaxIdle > cfg.MaxOpen {
		return nil, errors.New("invalid database connection limits")
	}
	if log == nil {
		log = slog.Default()
	}
	// GORM's automatic ping has no caller context; perform an explicit cancellable ping below.
	// TranslateError makes duplicate/foreign-key errors portable at the repository boundary.
	// Simple protocol avoids an additional implicit prepared-statement cache per connection.
	database, err := gorm.Open(dialector(cfg), &gorm.Config{
		DisableAutomaticPing: true, TranslateError: true, Logger: safeLogger{log: log, level: gormlogger.Warn},
	})
	if err != nil {
		return nil, ErrUnavailable
	}
	pool, err := database.DB()
	if err != nil {
		return nil, ErrUnavailable
	}
	// GORM holds the ORM state; database/sql owns pooling, connection limits and recycling.
	// Set limits before the first ping so every opened connection follows the same policy.
	pool.SetMaxOpenConns(cfg.MaxOpen)
	pool.SetMaxIdleConns(cfg.MaxIdle)
	pool.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	pool.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)
	for attempt := 1; attempt <= cfg.RetryAttempts; attempt++ {
		// PingContext provides sub-second bounds; connect_timeout also bounds driver setup.
		// Each attempt receives a fresh child context so an expired attempt cannot poison retries.
		attemptCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
		err = pool.PingContext(attemptCtx)
		// Cancel promptly even on success to release the child context's timer.
		cancel()
		if err == nil {
			return database, nil
		}
		if err := ctx.Err(); err != nil {
			_ = pool.Close()
			return nil, err
		}
		// Retry counters are safe observability fields; raw errors can include usernames or passwords.
		log.WarnContext(ctx, "database connection attempt failed", "attempt", attempt, "attempts", cfg.RetryAttempts)
		if attempt == cfg.RetryAttempts {
			break
		}
		// A timer plus select is cancellable, unlike time.Sleep inside a retry loop.
		// No new goroutine is needed to wait for either cancellation or retry readiness.
		timer := time.NewTimer(cfg.RetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = pool.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	// Failed startup must release its pool, otherwise a caller retry would retain resources.
	_ = pool.Close()
	return nil, ErrUnavailable
}

// dialector 使用已结构化的连接字段，密码不会通过拼接变成连接选项
// 每次 Open 创建独立连接池，Cluster 负责统一所有权和清理
func dialector(cfg configs.Database) gorm.Dialector {
	if cfg.EffectiveDriver() == "mysql" {
		// 初始化版本探测使用 Background，故关闭隐式探测，所有 I/O 由调用 context 管理
		// 本模板目标为 MySQL 8；不依赖 MariaDB 的 RETURNING 或旧版兼容行为
		return gormmysql.New(gormmysql.Config{DSN: mysqlDSN(cfg), SkipInitializeWithVersion: true})
	}
	return postgres.New(postgres.Config{DSN: connectionURL(cfg), PreferSimpleProtocol: true})
}

func mysqlDSN(cfg configs.Database) string {
	c := mysqldriver.NewConfig()
	c.Net, c.Addr, c.DBName = "tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)), cfg.Name
	c.User, c.Passwd = cfg.User, cfg.Password
	c.ParseTime, c.Loc, c.Timeout = true, time.UTC, cfg.ConnectTimeout
	// utf8mb4 支持完整 Unicode；二进制排序避免用户名/权限大小写被隐式合并
	c.Collation = "utf8mb4_bin"
	c.Params = map[string]string{"charset": "utf8mb4"}
	c.TLSConfig = cfg.TLS
	if c.TLSConfig == "" {
		c.TLSConfig = "true"
	}
	return c.FormatDSN()
}

// Constructing a URL, rather than concatenating key=value strings, preserves special
// characters in passwords and prevents values from becoming extra connection options.
func connectionURL(cfg configs.Database) string {
	u := &url.URL{Scheme: "postgres", User: url.UserPassword(cfg.User, cfg.Password), Host: net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)), Path: "/" + cfg.Name}
	q := url.Values{}
	q.Set("sslmode", cfg.SSLMode)
	// PostgreSQL's URL option uses whole seconds; round up rather than accidentally disabling it.
	// The explicit PingContext timeout remains precise for durations below one second.
	q.Set("connect_timeout", strconv.Itoa(max(1, int(math.Ceil(cfg.ConnectTimeout.Seconds())))))
	u.RawQuery = q.Encode()
	return u.String()
}

func Close(database *gorm.DB) error {
	// Nil-safe cleanup simplifies startup error paths without hiding errors after a real open.
	if database == nil {
		return nil
	}
	pool, err := database.DB()
	if err != nil {
		return ErrUnavailable
	}
	// Close prevents future queries and lets database/sql finish already running operations.
	if err := pool.Close(); err != nil {
		return ErrUnavailable
	}
	return nil
}

// Ping is deliberately separate from liveness: a temporary database outage should
// make the instance unready without forcing a container restart loop.
func Ping(ctx context.Context, database *gorm.DB) error {
	// Preserve context errors so shutdown and request cancellation remain distinguishable from outage.
	if ctx == nil {
		return errors.New("database context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if database == nil {
		return ErrUnavailable
	}
	pool, err := database.DB()
	if err != nil {
		return ErrUnavailable
	}
	if err := pool.PingContext(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrUnavailable
	}
	return nil
}

// Initialize is called by the init command or an explicitly enabled development flag.
// AutoMigrate creates/extends registered models; destructive/versioned changes remain
// a deployment responsibility instead of being hidden in ordinary HTTP startup.
func Initialize(ctx context.Context, database *gorm.DB, models []any) error {
	// Verify connectivity before DDL, even when no optional models have been registered yet.
	if err := Ping(ctx, database); err != nil {
		return err
	}
	if len(models) == 0 {
		return nil
	}
	// 驱动适配层以 advisory transaction lock / GET_LOCK 串行化初始化
	// MySQL DDL 隐式提交，不能像 PostgreSQL 一样承诺整段迁移原子回滚
	if err := initializeSchema(ctx, database, models); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// DDL errors can contain defaults or constraints with sensitive values; return a stable message.
		return errors.New("database schema initialization failed")
	}
	return nil
}

// CheckSchema reports missing tables without changing anything. Checking tables is
// intentionally narrower than proving every future migration has been applied.
func CheckSchema(ctx context.Context, database *gorm.DB, models []any) error {
	if err := Ping(ctx, database); err != nil {
		return err
	}
	if len(models) == 0 {
		return nil
	}
	// GetTables returns an error, unlike HasTable's boolean-only interface.
	// That distinction prevents a network failure from looking like a harmless absent table.
	tables, err := database.WithContext(ctx).Migrator().GetTables()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrSchema
	}
	exists := make(map[string]bool, len(tables))
	// Set membership keeps checking several registered modules linear in the number of tables.
	for _, table := range tables {
		exists[table] = true
	}
	for _, model := range models {
		// Parse uses GORM's own naming strategy, including explicit TableName overrides.
		// Reading registered model metadata does not issue SQL or mutate the database.
		statement := &gorm.Statement{DB: database}
		if err := statement.Parse(model); err != nil {
			return errors.New("invalid schema model registration")
		}
		if !exists[statement.Table] {
			return ErrSchema
		}
	}
	return ctx.Err()
}

// safeLogger discards SQL text and raw driver errors. GORM interpolated SQL can
// contain login hashes, runtime secrets, and user data even when debug mode is off.
type safeLogger struct {
	log   *slog.Logger
	level gormlogger.LogLevel
}

func (l safeLogger) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	// Return a copy: a request using Debug must not change the logger shared by other requests.
	l.level = level
	return l
}
func (l safeLogger) Info(ctx context.Context, _ string, _ ...any) {
	if l.level >= gormlogger.Info {
		l.log.DebugContext(ctx, "database operation")
	}
}
func (l safeLogger) Warn(ctx context.Context, _ string, _ ...any) {
	if l.level >= gormlogger.Warn {
		l.log.WarnContext(ctx, "database warning")
	}
}
func (l safeLogger) Error(ctx context.Context, _ string, _ ...any) {
	if l.level >= gormlogger.Error {
		l.log.ErrorContext(ctx, "database operation failed")
	}
}
func (l safeLogger) Trace(ctx context.Context, begin time.Time, _ func() (string, int64), err error) {
	// Do not call the SQL callback: rendering itself exposes parameters before any redaction step.
	if l.level == gormlogger.Silent {
		return
	}
	elapsed := time.Since(begin)
	// A missing record is expected control flow for lookups, not an infrastructure failure.
	// Duration remains useful for capacity diagnosis without including SQL or argument values.
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) && l.level >= gormlogger.Error {
		l.log.ErrorContext(ctx, "database operation failed", "duration_ms", elapsed.Milliseconds())
	} else if elapsed > 200*time.Millisecond && l.level >= gormlogger.Warn {
		l.log.WarnContext(ctx, "slow database operation", "duration_ms", elapsed.Milliseconds())
	}
}

// Keep String safe if the logger is accidentally inspected during diagnostics.
func (l safeLogger) String() string { return fmt.Sprintf("database logger (level %d)", l.level) }

package testutil

import (
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Each real database test gets its own PostgreSQL schema or MySQL database.
// A privileged test DSN is deliberate: it must never point at production data.
// PostgreSQL wins when both variables exist, so CI runs separate jobs per driver.
func OpenDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	postgresDSN := os.Getenv("HEAGO_TEST_DATABASE_URL")
	mysqlDSN := os.Getenv("HEAGO_TEST_MYSQL_DSN")
	if postgresDSN == "" && mysqlDSN == "" {
		t.Skip("set HEAGO_TEST_DATABASE_URL or HEAGO_TEST_MYSQL_DSN for real database tests")
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal("generate isolated database name")
	}
	name := "heago_test_" + hex.EncodeToString(random[:])
	var adminDialector, isolatedDialector gorm.Dialector
	var createSQL, dropSQL string
	if postgresDSN != "" {
		adminDialector = postgres.Open(postgresDSN)
		isolatedDSN := postgresDSN + " search_path=" + name
		if strings.Contains(postgresDSN, "://") {
			parsed, err := url.Parse(postgresDSN)
			if err != nil {
				t.Fatal("parse test PostgreSQL URL")
			}
			query := parsed.Query()
			query.Set("search_path", name)
			parsed.RawQuery = query.Encode()
			isolatedDSN = parsed.String()
		}
		isolatedDialector = postgres.Open(isolatedDSN)
		createSQL = "CREATE SCHEMA " + name
		dropSQL = "DROP SCHEMA " + name + " CASCADE"
	} else {
		config, err := mysqldriver.ParseDSN(mysqlDSN)
		if err != nil {
			t.Fatal("parse test MySQL DSN")
		}
		// Driver timestamps must be parsed as time.Time on every pooled connection.
		// Binary collation preserves PostgreSQL's case-sensitive identity names.
		config.ParseTime, config.Loc, config.Collation = true, time.UTC, "utf8mb4_bin"
		adminDialector = mysql.Open(config.FormatDSN())
		config.DBName = name
		isolatedDialector = mysql.Open(config.FormatDSN())
		createSQL = "CREATE DATABASE " + name + " CHARACTER SET utf8mb4 COLLATE utf8mb4_bin"
		dropSQL = "DROP DATABASE " + name
	}
	ormConfig := func() *gorm.Config { return &gorm.Config{Logger: logger.Discard, TranslateError: true} }
	admin, err := gorm.Open(adminDialector, ormConfig())
	if err != nil {
		t.Fatal("open test administrative database")
	}
	adminSQL, err := admin.DB()
	if err != nil {
		t.Fatal("obtain test administrative connection")
	}
	t.Cleanup(func() { _ = adminSQL.Close() })
	// DDL identifiers come entirely from a fixed prefix plus random hex bytes.
	// Credentials are only passed to the driver; failures never print the DSN.
	if err := admin.Exec(createSQL).Error; err != nil {
		t.Fatal("create isolated test database")
	}
	t.Cleanup(func() {
		if err := admin.Exec(dropSQL).Error; err != nil {
			t.Error("drop isolated test database")
		}
	})
	database, err := gorm.Open(isolatedDialector, ormConfig())
	if err != nil {
		t.Fatal("open isolated test database")
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatal("obtain isolated test connection")
	}
	sqlDB.SetMaxOpenConns(8)
	t.Cleanup(func() { _ = sqlDB.Close() })
	// Tables inherit the isolated database's binary collation and use a
	// transactional engine; the rollback tests rely on real InnoDB behavior.
	if database.Dialector.Name() == "mysql" {
		database = database.Set("gorm:table_options", "ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin")
	}
	// Set returns a mutable chain. Start a reusable session before handing the
	// handle to tests, otherwise one direct query can leave Table/Where state
	// behind and accidentally affect the next independent operation.
	return database.Session(&gorm.Session{})
}

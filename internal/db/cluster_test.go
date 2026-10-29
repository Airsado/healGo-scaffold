package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

// 只重试真正的连接故障；查询语义错误重试会掩盖问题并放大主库负载
// 特别覆盖 DeadlineExceeded 的 net.Error 行为，防止耗尽预算后再次查询
func TestConnectionFailureClassification(t *testing.T) {
	for name, tc := range map[string]struct {
		err   error
		retry bool
	}{
		"bad connection":           {driver.ErrBadConn, true},
		"mysql invalid connection": {mysqldriver.ErrInvalidConn, true},
		"closed connection":        {sql.ErrConnDone, true},
		"eof":                      {io.EOF, true},
		"wrapped eof":              {fmt.Errorf("read: %w", io.ErrUnexpectedEOF), true},
		"network":                  {&net.OpError{Op: "read", Net: "tcp", Err: errors.New("reset")}, true},
		"postgres shutdown":        {&pgconn.PgError{Code: "57P01"}, true},
		"postgres connection":      {&pgconn.PgError{Code: "08006"}, true},
		"mysql lost connection":    {&mysqldriver.MySQLError{Number: 2013}, true},
		"not found":                {gorm.ErrRecordNotFound, false},
		"no rows":                  {sql.ErrNoRows, false},
		"cancelled":                {context.Canceled, false},
		"deadline":                 {fmt.Errorf("query: %w", context.DeadlineExceeded), false},
		"postgres syntax":          {&pgconn.PgError{Code: "42601"}, false},
		"mysql permission":         {&mysqldriver.MySQLError{Number: 1142}, false},
		"unknown":                  {errors.New("unknown SQL failure"), false},
	} {
		t.Run(name, func(t *testing.T) {
			if connectionFailure(tc.err) != tc.retry {
				t.Fatal("wrong retry classification")
			}
		})
	}
}

func TestClusterRejectsInvalidReadsAndCloseIsIdempotent(t *testing.T) {
	var cluster *Cluster
	if err := cluster.Close(); err != nil {
		t.Fatal(err)
	}
	called := false
	query := func(*gorm.DB) error { called = true; return nil }
	if err := cluster.Read(context.Background(), query); !errors.Is(err, ErrUnavailable) {
		t.Fatal("nil cluster accepted")
	}
	if err := cluster.Read(nil, query); err == nil {
		t.Fatal("nil context accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cluster.Read(ctx, query); !errors.Is(err, context.Canceled) {
		t.Fatal("cancel ignored")
	}
	if called {
		t.Fatal("invalid read invoked query")
	}
	cluster = &Cluster{}
	if cluster.Close() != nil || cluster.Close() != nil {
		t.Fatal("duplicate cleanup failed")
	}
}

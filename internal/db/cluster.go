package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"

	"heago-scaffold/configs"

	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

// Cluster 拥有主库和可选的一个副本连接池Primary 不安装自动路由插件，
// 认证、配置、事务和普通 Mapper 因此天然读写主库，不受复制延迟影响
type Cluster struct {
	Primary   *gorm.DB
	replica   *gorm.DB
	fallback  bool
	log       *slog.Logger
	reads     atomic.Uint64
	fallbacks atomic.Uint64
	closed    atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

// ReadStats 是进程内累积计数，不包含 SQL、参数或凭据
// ReplicaAttempts 包含失败尝试；PrimaryFallbacks 只统计真正执行的回退
type ReadStats struct{ ReplicaAttempts, PrimaryFallbacks uint64 }

func (c *Cluster) Stats() ReadStats {
	if c == nil {
		return ReadStats{}
	}
	return ReadStats{c.reads.Load(), c.fallbacks.Load()}
}

// OpenCluster 严格检查已配置节点：副本启动失败也关闭主库并拒绝启动
// 运行期间的副本连接故障可按配置回退；这不等于自动选举或提升主库
func OpenCluster(ctx context.Context, cfg configs.Database, log *slog.Logger) (*Cluster, error) {
	if len(cfg.Replicas) > 1 {
		return nil, errors.New("at most one database replica is supported")
	}
	if log == nil {
		log = slog.Default()
	}
	primary, err := Open(ctx, cfg, log)
	if err != nil {
		return nil, err
	}
	c := &Cluster{Primary: primary, fallback: cfg.FallbackToPrimary, log: log}
	for _, replica := range cfg.ReplicaConfigs() {
		c.replica, err = Open(ctx, replica, log)
		if err != nil {
			_ = c.Close()
			return nil, err
		}
	}
	return c, nil
}

// Read 仅供允许最终一致性的幂等、纯查询回调可能执行两次，必须在每次
// 进入时重置结果；不能在其中写库、发消息或产生其他不可重复的副作用
// 需要同一快照/写后立即读的用例直接使用 Primary 或 Primary.Transaction
func (c *Cluster) Read(ctx context.Context, fn func(*gorm.DB) error) error {
	if ctx == nil || fn == nil {
		return errors.New("read context and callback are required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if c == nil || c.Primary == nil || c.closed.Load() {
		return ErrUnavailable
	}
	if c.replica == nil {
		return fn(c.Primary.WithContext(ctx))
	}
	c.reads.Add(1)
	err := fn(c.replica.WithContext(ctx))
	// 空列表、not found、SQL/权限错误均是查询结果，不能通过回主库掩盖
	// 父 context 预算已用尽时也不能重新开启一轮 I/O
	if err == nil || !c.fallback || ctx.Err() != nil || !connectionFailure(err) {
		return err
	}
	c.fallbacks.Add(1)
	c.log.WarnContext(ctx, "database_replica_read_fallback", "node_role", "replica", "attempt", 1)
	return fn(c.Primary.WithContext(ctx))
}

// Close 按构造的逆序释放两个池，并发/重复关闭也只执行一次
// handle 不会创建额外的路由池或拥有隐式后台任务
func (c *Cluster) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		if err := Close(c.replica); err != nil {
			c.closeErr = err
		}
		if err := Close(c.Primary); err != nil {
			c.closeErr = err
		}
	})
	return c.closeErr
}

func connectionFailure(err error) bool {
	// DeadlineExceeded 也实现 net.Error；必须先排除，不能把超时查询重放
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, mysqldriver.ErrInvalidConn) || errors.Is(err, sql.ErrConnDone) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var network net.Error
	if errors.As(err, &network) {
		return true
	}
	var state interface{ SQLState() string }
	if errors.As(err, &state) {
		code := state.SQLState()
		return len(code) >= 2 && code[:2] == "08" || code == "57P01" || code == "57P02" || code == "57P03"
	}
	var mysql *mysqldriver.MySQLError
	if errors.As(err, &mysql) {
		return mysql.Number == 2006 || mysql.Number == 2013
	}
	return false
}

package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"math"
	"strconv"
	"time"

	"gorm.io/gorm"
)

// LockedTransaction 在主库串行执行需要跨进程互斥的业务事务
// PG 锁跟随事务结束；MySQL GET_LOCK 是会话锁，必须覆盖提交并显式释放
// 普通的多表用例仍可直接 database.WithContext(ctx).Transaction，不必都加锁
func LockedTransaction(ctx context.Context, database *gorm.DB, key int64, fn func(*gorm.DB) error) error {
	if ctx == nil || database == nil || fn == nil {
		return errors.New("database transaction arguments are required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if database.Dialector.Name() == "mysql" {
		return mysqlLocked(ctx, database, key, func(conn *gorm.DB) error { return conn.Transaction(fn) })
	}
	return database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", key).Error; err != nil {
			return err
		}
		return fn(tx)
	})
}

func initializeSchema(ctx context.Context, database *gorm.DB, models []any) error {
	const schemaLock int64 = 0x484541474f534348
	if database.Dialector.Name() == "mysql" {
		// MySQL DDL 隐式提交，不能承诺整个 AutoMigrate 可事务回滚
		// 会话锁仍覆盖完整初始化过程，防止多个部署实例同时修改 schema
		return mysqlLocked(ctx, database, schemaLock, func(conn *gorm.DB) error {
			return conn.Set("gorm:table_options", "ENGINE=InnoDB CHARACTER SET utf8mb4 COLLATE utf8mb4_bin").AutoMigrate(models...)
		})
	}
	return LockedTransaction(ctx, database, schemaLock, func(tx *gorm.DB) error { return tx.AutoMigrate(models...) })
}

func mysqlLocked(ctx context.Context, database *gorm.DB, key int64, fn func(*gorm.DB) error) error {
	// Connection 固定同一物理连接；从池中分别执行 GET_LOCK/RELEASE_LOCK 是错误的
	return database.WithContext(ctx).Connection(func(conn *gorm.DB) (err error) {
		var name string
		if err := conn.Session(&gorm.Session{NewDB: true}).Raw("SELECT DATABASE()").Scan(&name).Error; err != nil {
			return err
		}
		hash := sha256.Sum256([]byte(name))
		lockName := "heago:" + hex.EncodeToString(hash[:12]) + ":" + strconv.FormatInt(key, 16)
		// 数据库名参与锁名，多个独立测试库或服务不会互相占用同一锁
		wait := 10.0
		if deadline, ok := ctx.Deadline(); ok {
			wait = math.Min(wait, math.Max(0, time.Until(deadline).Seconds()))
		}
		var acquired sql.NullInt64
		if err := conn.Session(&gorm.Session{NewDB: true}).Raw("SELECT GET_LOCK(?, ?)", lockName, wait).Scan(&acquired).Error; err != nil {
			return err
		}
		if !acquired.Valid || acquired.Int64 != 1 {
			return errors.New("database lock unavailable")
		}
		defer func() {
			// 请求已取消也必须尝试解锁；保留独立短预算，不无限延长停机
			cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var released sql.NullInt64
			unlockErr := conn.Session(&gorm.Session{NewDB: true}).WithContext(cleanupCtx).Raw("SELECT RELEASE_LOCK(?)", lockName).Scan(&released).Error
			if unlockErr != nil || !released.Valid || released.Int64 != 1 {
				// 若锁无法确认释放，不能把可能仍持锁的会话放回连接池
				if raw, ok := conn.Statement.ConnPool.(*sql.Conn); ok {
					_ = raw.Raw(func(any) error { return driver.ErrBadConn })
				}
				if err == nil {
					err = errors.New("database lock cleanup failed")
				}
			}
		}()
		// 独立 Statement 防止探针的 Scan 目标/表名污染业务 SQL；ConnPool 仍固定同一会话
		return fn(conn.Session(&gorm.Session{NewDB: true}))
	})
}

package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	database "heago-scaffold/internal/db"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestIdentityLockCancellationReleasesAcrossIndependentPools(t *testing.T) {
	first := isolatedDB(t)
	// A second GORM open creates a separate database/sql pool. Reusing the
	// original pool could hide a leaked recursive MySQL GET_LOCK on one session.
	second, err := gorm.Open(first.Dialector, &gorm.Config{Logger: logger.Discard, TranslateError: true})
	if err != nil {
		t.Fatal("open independent identity database pool")
	}
	secondPool, err := second.DB()
	if err != nil {
		t.Fatal("obtain independent identity database pool")
	}
	t.Cleanup(func() { _ = secondPool.Close() })
	holderContext, cancelHolder := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelHolder()
	entered := make(chan struct{})
	release := make(chan struct{})
	holderResult := make(chan error, 1)
	holderDone := make(chan struct{})
	var releaseOnce sync.Once
	stopHolder := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		stopHolder()
		select {
		case <-holderDone:
		case <-time.After(2 * time.Second):
			t.Error("identity lock holder did not finish during cleanup")
		}
	})
	go func() {
		defer close(holderDone)
		holderResult <- database.LockedTransaction(holderContext, first, identityLock, func(*gorm.DB) error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-holderContext.Done():
				return holderContext.Err()
			}
		})
	}()
	select {
	case <-entered:
	case err := <-holderResult:
		t.Fatal("first identity lock was not acquired", err)
	case <-holderContext.Done():
		t.Fatal("first identity lock acquisition timed out")
	}
	// Cancellation while waiting must return promptly and must never run the
	// protected mutation. GET_LOCK timeout and context deadline are both failures.
	waitContext, cancelWait := context.WithTimeout(context.Background(), 100*time.Millisecond)
	started := time.Now()
	called := false
	err = database.LockedTransaction(waitContext, second, identityLock, func(*gorm.DB) error {
		called = true
		return nil
	})
	cancelWait()
	if err == nil || called || time.Since(started) > time.Second {
		t.Fatal("waiting identity transaction ignored cancellation or mutex")
	}
	stopHolder()
	if err := <-holderResult; err != nil {
		t.Fatal("first identity transaction failed after releasing its waiter", err)
	}
	// Cancellation after acquisition exercises the cleanup path with an already
	// canceled request context. Unlock must use an independent bounded context.
	mutationContext, cancelMutation := context.WithCancel(context.Background())
	err = database.LockedTransaction(mutationContext, first, identityLock, func(*gorm.DB) error {
		cancelMutation()
		return mutationContext.Err()
	})
	defer cancelMutation()
	if !errors.Is(err, context.Canceled) {
		t.Fatal("canceled identity mutation did not roll back")
	}
	verifyContext, cancelVerify := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelVerify()
	if err := database.LockedTransaction(verifyContext, second, identityLock, func(*gorm.DB) error { return nil }); err != nil {
		t.Fatal("canceled identity transaction leaked a database lock", err)
	}
}

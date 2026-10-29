package mapper_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"healgo-scaffold/internal/testutil"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	model "healgo-scaffold/internal/model/note"
	"healgo-scaffold/pkg/serve/mapper"
	"healgo-scaffold/pkg/serve/service"
	"healgo-scaffold/pkg/serve/service/impl"

	"gorm.io/gorm"
)

func testDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	database := testutil.OpenDatabase(t)
	if err := database.AutoMigrate(&model.Note{}); err != nil {
		t.Fatal("initialize isolated test tables")
	}
	return database
}

func testService(t *testing.T) service.NoteService {
	t.Helper()
	store, err := mapper.NewNoteMapper(testDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	return impl.NewNoteService(store)
}

func TestCRUDOwnerIsolationAndRevision(t *testing.T) {
	s := testService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	note, err := s.Create(ctx, 11, service.CreateNoteInput{Slug: "daily-note", Title: "  中文笔记  ", Content: "first"})
	if err != nil {
		t.Fatal(err)
	}
	if note.ID == 0 || note.OwnerID != 11 || note.Title != "中文笔记" || note.Revision != 1 || note.CreatedAt.IsZero() || note.UpdatedAt.IsZero() {
		t.Fatal("create did not persist normalized fields")
	}
	if _, err := s.Create(ctx, 11, service.CreateNoteInput{Slug: "daily-note", Title: "duplicate"}); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("duplicate slug returned %v", err)
	}
	other, err := s.Create(ctx, 22, service.CreateNoteInput{Slug: "daily-note", Title: "other owner"})
	if err != nil {
		t.Fatal("different owners must be allowed the same slug")
	}
	if _, err := s.Get(ctx, 22, note.ID); !errors.Is(err, service.ErrNotFound) {
		t.Fatal("cross-owner read revealed note")
	}
	if _, err := s.Update(ctx, 22, note.ID, service.UpdateNoteInput{Title: "intrusion", Revision: 1}); !errors.Is(err, service.ErrNotFound) {
		t.Fatal("cross-owner update revealed or changed note")
	}
	if err := s.Delete(ctx, 22, note.ID, 1); !errors.Is(err, service.ErrNotFound) {
		t.Fatal("cross-owner delete revealed or removed note")
	}
	second, err := s.Create(ctx, 11, service.CreateNoteInput{Slug: "next", Title: "second"})
	if err != nil {
		t.Fatal(err)
	}
	firstPage, err := s.List(ctx, 11, 1, 1)
	if err != nil || firstPage.Total != 2 || len(firstPage.Items) != 1 || firstPage.Items[0].ID != second.ID {
		t.Fatal("first page is not stable or owner-scoped")
	}
	nextPage, err := s.List(ctx, 11, 2, 1)
	if err != nil || len(nextPage.Items) != 1 || nextPage.Items[0].ID != note.ID {
		t.Fatal("second page differs from deterministic ID ordering")
	}
	otherPage, err := s.List(ctx, 22, 1, 20)
	if err != nil || otherPage.Total != 1 || len(otherPage.Items) != 1 || otherPage.Items[0].ID != other.ID {
		t.Fatal("list leaked another owner's rows or count")
	}
	empty, err := s.List(ctx, 11, 4, 20)
	if err != nil || empty.Items == nil || len(empty.Items) != 0 {
		t.Fatal("empty page must be an empty collection")
	}
	updated, err := s.Update(ctx, 11, note.ID, service.UpdateNoteInput{Title: "  已修改  ", Content: "", Revision: 1})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != note.ID || updated.OwnerID != 11 || updated.Slug != note.Slug || updated.Revision != 2 || updated.Title != "已修改" || updated.Content != "" || updated.CreatedAt.UnixMicro() != note.CreatedAt.UnixMicro() {
		t.Fatal("CAS update did not return the saved row or clear content")
	}
	if _, err := s.Update(ctx, 11, note.ID, service.UpdateNoteInput{Title: "stale write", Revision: 1}); !errors.Is(err, service.ErrConflict) {
		t.Fatal("stale update overwrote new data")
	}
	if err := s.Delete(ctx, 11, note.ID, 1); !errors.Is(err, service.ErrConflict) {
		t.Fatal("stale delete removed a newer revision")
	}
	retained, err := s.Get(ctx, 11, note.ID)
	if err != nil || retained.Revision != 2 || retained.Title != "已修改" {
		t.Fatal("stale operation changed saved data")
	}
	if err := s.Delete(ctx, 11, note.ID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, 11, note.ID); !errors.Is(err, service.ErrNotFound) {
		t.Fatal("deleted row is still readable")
	}
}

func TestConcurrentCASHasOneWinner(t *testing.T) {
	s := testService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	note, err := s.Create(ctx, 71, service.CreateNoteInput{Slug: "race", Title: "initial"})
	if err != nil {
		t.Fatal(err)
	}
	const attempts = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	var won, conflicts atomic.Int32
	unexpected := make(chan error, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			_, err := s.Update(ctx, 71, note.ID, service.UpdateNoteInput{Title: fmt.Sprintf("writer-%d", index), Content: fmt.Sprintf("content-%d", index), Revision: 1})
			switch {
			case err == nil:
				won.Add(1)
			case errors.Is(err, service.ErrConflict):
				conflicts.Add(1)
			default:
				unexpected <- err
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(unexpected)
	for err := range unexpected {
		t.Errorf("unexpected CAS error: %v", err)
	}
	if won.Load() != 1 || conflicts.Load() != attempts-1 {
		t.Fatal("CAS did not reject every losing writer")
	}
	final, err := s.Get(ctx, 71, note.ID)
	if err != nil || final.Revision != 2 {
		t.Fatal("concurrent update incremented revision more than once")
	}
	if final.Title == "initial" || final.Content == "" {
		t.Fatal("winner's payload was not saved")
	}
}

func TestCanceledContextStopsAllPersistence(t *testing.T) {
	s := testService(t)
	note, err := s.Create(context.Background(), 81, service.CreateNoteInput{Slug: "existing", Title: "existing"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	checks := []func() error{
		func() error {
			_, err := s.Create(ctx, 81, service.CreateNoteInput{Slug: "canceled", Title: "not inserted"})
			return err
		},
		func() error { _, err := s.Get(ctx, 81, note.ID); return err },
		func() error { _, err := s.List(ctx, 81, 1, 20); return err },
		func() error {
			_, err := s.Update(ctx, 81, note.ID, service.UpdateNoteInput{Title: "not saved", Revision: 1})
			return err
		},
		func() error { return s.Delete(ctx, 81, note.ID, 1) },
	}
	for _, check := range checks {
		if err := check(); !errors.Is(err, context.Canceled) || !errors.Is(err, service.ErrUnavailable) {
			t.Fatalf("canceled persistence returned %v", err)
		}
	}
	page, err := s.List(context.Background(), 81, 1, 20)
	if err != nil || page.Total != 1 || page.Items[0].Revision != 1 {
		t.Fatal("canceled operation changed data")
	}
}

func TestConstructorDoesNotQueryOrAcceptNil(t *testing.T) {
	if _, err := mapper.NewNoteMapper(nil); !errors.Is(err, model.ErrInvalidInput) {
		t.Fatal("nil database accepted")
	}
	if _, err := mapper.NewNoteMapper(&gorm.DB{}); err != nil {
		t.Fatal("constructor performed database I/O")
	}
}

// 两个 Mapper 必须使用同一个 tx；列表也要看到未提交数据，不能逃到副本/主池
// 主动回滚后检查库中没有残留，覆盖 MySQL 嵌套更新事务的 savepoint 行为
func TestSharedTransactionSeesOwnWritesAndRollsBack(t *testing.T) {
	database := testDatabase(t)
	ctx := context.Background()
	rollback := errors.New("rollback test transaction")
	err := database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		first, _ := mapper.NewNoteMapper(tx)
		second, _ := mapper.NewNoteMapper(tx)
		note, err := first.Create(ctx, model.Note{OwnerID: 91, Slug: "transaction", Title: "before"})
		if err != nil {
			return err
		}
		if _, err := second.Get(ctx, 91, note.ID); err != nil {
			return err
		}
		if _, total, err := second.List(ctx, 91, 0, 10); err != nil || total != 1 {
			return errors.New("transaction list escaped its connection")
		}
		updated, err := first.Update(ctx, 91, note.ID, 1, "after", "")
		if err != nil {
			return err
		}
		if updated.Revision != 2 || updated.Title != "after" {
			return errors.New("transaction update returned wrong result")
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal("shared transaction failed", err)
	}
	store, _ := mapper.NewNoteMapper(database)
	if _, total, err := store.List(ctx, 91, 0, 10); err != nil || total != 0 {
		t.Fatal("rolled back transaction left rows")
	}
}

// MySQL 默认可重复读可能保留旧 SELECT 快照CAS 失败后的分类必须使用
// current read，否则它可能把新版本读回成旧版本，或误报资源不存在
func TestMySQLConflictUsesCurrentReadAfterOlderSnapshot(t *testing.T) {
	database := testDatabase(t)
	if database.Dialector.Name() != "mysql" {
		t.Skip("MySQL repeatable-read semantics")
	}
	ctx := context.Background()
	writer, _ := mapper.NewNoteMapper(database)
	note, err := writer.Create(ctx, model.Note{OwnerID: 92, Slug: "snapshot", Title: "initial"})
	if err != nil {
		t.Fatal(err)
	}
	err = database.Transaction(func(tx *gorm.DB) error {
		reader, _ := mapper.NewNoteMapper(tx)
		if _, err := reader.Get(ctx, 92, note.ID); err != nil {
			return err
		}
		if _, err := writer.Update(ctx, 92, note.ID, 1, "committed", ""); err != nil {
			return err
		}
		if _, err := reader.Update(ctx, 92, note.ID, 1, "stale", ""); !errors.Is(err, model.ErrConflict) {
			return errors.New("stale snapshot caused wrong conflict classification")
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	got, err := writer.Get(ctx, 92, note.ID)
	if err != nil || got.Revision != 2 || got.Title != "committed" {
		t.Fatal("stale update changed committed state")
	}
}

func TestMySQLDeleteClassificationIgnoresOldSnapshot(t *testing.T) {
	database := testDatabase(t)
	if database.Dialector.Name() != "mysql" {
		t.Skip("MySQL repeatable-read semantics")
	}
	ctx := context.Background()
	writer, _ := mapper.NewNoteMapper(database)
	note, err := writer.Create(ctx, model.Note{OwnerID: 93, Slug: "deleted-snapshot", Title: "initial"})
	if err != nil {
		t.Fatal(err)
	}
	// 先建立旧快照，再由另一个已提交事务删除；普通Get仍可能看到旧行
	// Delete 的失败分类要以当前行状态返回404，而非根据旧快照误报409
	err = database.Transaction(func(tx *gorm.DB) error {
		reader, _ := mapper.NewNoteMapper(tx)
		if _, err := reader.Get(ctx, 93, note.ID); err != nil {
			return err
		}
		if err := writer.Delete(ctx, 93, note.ID, 1); err != nil {
			return err
		}
		if err := reader.Delete(ctx, 93, note.ID, 1); !errors.Is(err, model.ErrNotFound) {
			return errors.New("delete used stale snapshot for missing row")
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
}

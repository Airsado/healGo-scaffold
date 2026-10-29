package impl

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	model "heago-scaffold/internal/model/note"
	"heago-scaffold/pkg/serve/service"
)

type memoryMapper struct {
	calls               int
	created             model.Note
	owner, id, revision uint64
	offset, limit       int
	title, content      string
	ctx                 context.Context
}

func (m *memoryMapper) Create(ctx context.Context, n model.Note) (model.Note, error) {
	m.calls++
	m.created = n
	m.ctx = ctx
	return n, nil
}
func (m *memoryMapper) Get(ctx context.Context, owner, id uint64) (model.Note, error) {
	m.calls++
	m.owner = owner
	m.id = id
	m.ctx = ctx
	return model.Note{ID: id, OwnerID: owner}, nil
}
func (m *memoryMapper) List(ctx context.Context, owner uint64, offset, limit int) ([]model.Note, int64, error) {
	m.calls++
	m.owner = owner
	m.offset = offset
	m.limit = limit
	m.ctx = ctx
	return nil, 0, nil
}
func (m *memoryMapper) Update(ctx context.Context, owner, id, revision uint64, title, content string) (model.Note, error) {
	m.calls++
	m.owner = owner
	m.id = id
	m.revision = revision
	m.title = title
	m.content = content
	m.ctx = ctx
	return model.Note{ID: id, OwnerID: owner, Title: title, Content: content, Revision: revision + 1}, nil
}
func (m *memoryMapper) Delete(ctx context.Context, owner, id, revision uint64) error {
	m.calls++
	m.owner = owner
	m.id = id
	m.revision = revision
	m.ctx = ctx
	return nil
}

func TestCreateValidationPreventsPersistenceAndNormalizesTitle(t *testing.T) {
	m := &memoryMapper{}
	s := NewNoteService(m)
	valid := service.CreateNoteInput{Slug: "daily-note-1", Title: "  中文标题  ", Content: strings.Repeat("文", 10000)}
	ctx := context.WithValue(context.Background(), struct{}{}, "trace")
	if _, err := s.Create(ctx, 7, valid); err != nil {
		t.Fatal(err)
	}
	if m.created.Title != "中文标题" || m.created.OwnerID != 7 || m.ctx != ctx || m.created.Revision != 1 {
		t.Fatal("service changed identity/context or failed normalization")
	}
	cases := []struct {
		name   string
		owner  uint64
		change func(*service.CreateNoteInput)
	}{
		{"missing owner", 0, func(*service.CreateNoteInput) {}},
		{"overflow owner", math.MaxUint64, func(*service.CreateNoteInput) {}},
		{"uppercase slug", 7, func(i *service.CreateNoteInput) { i.Slug = "Daily" }},
		{"repeated hyphen", 7, func(i *service.CreateNoteInput) { i.Slug = "daily--note" }},
		{"leading hyphen", 7, func(i *service.CreateNoteInput) { i.Slug = "-daily" }},
		{"long slug", 7, func(i *service.CreateNoteInput) { i.Slug = strings.Repeat("a", 65) }},
		{"empty title", 7, func(i *service.CreateNoteInput) { i.Title = " \t " }},
		{"long unicode title", 7, func(i *service.CreateNoteInput) { i.Title = strings.Repeat("文", 121) }},
		{"long content", 7, func(i *service.CreateNoteInput) { i.Content = strings.Repeat("文", 10001) }},
		{"NUL content", 7, func(i *service.CreateNoteInput) { i.Content = "a\x00b" }},
		{"invalid UTF8", 7, func(i *service.CreateNoteInput) { i.Content = string([]byte{0xff}) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := m.calls
			input := valid
			tc.change(&input)
			if _, err := s.Create(ctx, tc.owner, input); !errors.Is(err, service.ErrInvalidInput) {
				t.Fatalf("invalid input returned %v", err)
			}
			if m.calls != before {
				t.Fatal("invalid input reached mapper")
			}
		})
	}
}

func TestRevisionPaginationAndIdentityBoundaries(t *testing.T) {
	m := &memoryMapper{}
	s := NewNoteService(m)
	ctx := context.Background()
	for _, revision := range []uint64{0, math.MaxInt64, math.MaxUint64} {
		if _, err := s.Update(ctx, 1, 2, service.UpdateNoteInput{Title: "valid", Revision: revision}); !errors.Is(err, service.ErrInvalidInput) {
			t.Fatal("invalid revision accepted")
		}
	}
	updated, err := s.Update(ctx, 1, 2, service.UpdateNoteInput{Title: "  updated  ", Content: "", Revision: 3})
	if err != nil || updated.Title != "updated" || m.owner != 1 || m.id != 2 || m.revision != 3 || m.content != "" {
		t.Fatal("update did not preserve scope or empty content")
	}
	for _, page := range []int{0, -1, 10001, math.MaxInt} {
		if _, err := s.List(ctx, 1, page, 100); !errors.Is(err, service.ErrInvalidInput) {
			t.Fatal("invalid page accepted")
		}
	}
	for _, size := range []int{0, -1, 101} {
		if _, err := s.List(ctx, 1, 1, size); !errors.Is(err, service.ErrInvalidInput) {
			t.Fatal("invalid page size accepted")
		}
	}
	page, err := s.List(ctx, 1, 10000, 100)
	if err != nil || m.offset != 999900 || m.limit != 100 || page.Items == nil || page.Page != 10000 || page.PageSize != 100 {
		t.Fatal("pagination overflowed or returned nil items")
	}
	if _, err := s.Get(ctx, 0, 2); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatal("owner zero accepted")
	}
	if err := s.Delete(ctx, 1, 2, 0); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatal("delete without revision accepted")
	}
	if err := s.Delete(ctx, 1, 2, 3); err != nil || m.owner != 1 || m.id != 2 || m.revision != 3 {
		t.Fatal("delete changed identity scope")
	}
	if _, err := NewNoteService(nil).Get(ctx, 1, 2); !errors.Is(err, service.ErrUnavailable) {
		t.Fatal("nil mapper should fail closed")
	}
}

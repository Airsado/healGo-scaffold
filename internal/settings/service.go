package settings

import (
	"context"
	"crypto/cipher"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Service does not cache values: persisted revisions are authoritative across
// multiple API instances. The registry is copied once and immutable thereafter.
type Service struct {
	db          *gorm.DB
	cipher      cipher.AEAD
	definitions map[string]Definition
	keys        []string
}

func New(db *gorm.DB, encryptionKey string, definitions []Definition) (*Service, error) {
	// Construction only validates the registry. Actual schema creation and I/O
	// remain explicit bootstrap steps, making initialization order reviewable.
	if db == nil {
		return nil, ErrInvalidInput
	}
	aead, err := newCipher(encryptionKey)
	if err != nil {
		return nil, err
	}
	s := &Service{db: db, cipher: aead, definitions: make(map[string]Definition, len(definitions))}
	for _, def := range definitions {
		if !validKey(def.Key) || len(def.Description) > 1024 {
			return nil, ErrInvalidInput
		}
		if _, exists := s.definitions[def.Key]; exists {
			return nil, ErrInvalidInput
		}
		value, err := normalized(def.Default)
		if err != nil || valueType(value) == 'n' || (def.Secret && valueType(value) != '"') {
			return nil, ErrInvalidInput
		}
		def.Default = value
		// Normalization allocated new bytes, so a caller mutating its original
		// RawMessage after New cannot alter a future missing-row seed.
		if _, err := validate(def, value, false); err != nil {
			return nil, ErrInvalidInput
		}
		s.definitions[def.Key] = def
		s.keys = append(s.keys, def.Key)
	}
	// A stable order makes initialization lock acquisition and administrative
	// listings consistent across instances even if registration order differs.
	sort.Strings(s.keys)
	return s, nil
}

// Initialize seeds missing rows and verifies every registered stored value in one
// transaction. A bad key, invalid stored JSON, or changed secret classification
// fails startup instead of replacing persisted configuration with defaults.
func (s *Service) Initialize(ctx context.Context) error {
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, key := range s.keys {
			def := s.definitions[key]
			payload := append([]byte(nil), def.Default...)
			if def.Secret {
				var err error
				payload, err = s.seal(key, payload)
				if err != nil {
					return err
				}
			}
			row := record{Key: key, Payload: payload, Secret: def.Secret, Revision: 1}
			// Concurrent startup is safe: one instance inserts and every instance
			// then verifies the persisted row. No seed update can erase user edits.
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
				return ErrUnavailable
			}
			if err := tx.Where(clause.Eq{Column: "key", Value: key}).First(&row).Error; err != nil {
				return ErrUnavailable
			}
			if _, err := s.plain(def, row); err != nil {
				return err
			}
		}
		return nil
	})
	return storageError(err)
}

func (s *Service) plain(def Definition, row record) (json.RawMessage, error) {
	// Secret classification belongs to code registration, not mutable data.
	// Refusing mismatches prevents a corrupted flag from exposing ciphertext as
	// ordinary JSON or treating a plaintext row as an authenticated credential.
	if row.Secret != def.Secret || row.Revision == 0 || row.Revision > math.MaxInt64 {
		return nil, ErrUnavailable
	}
	payload := append([]byte(nil), row.Payload...)
	if def.Secret {
		var err error
		payload, err = s.open(def.Key, payload)
		if err != nil {
			return nil, ErrUnavailable
		}
	}
	value, err := validate(def, payload, false)
	// Stored data is validated on reads as well as writes. A manual database
	// edit therefore cannot bypass a typed definition or a newer safety rule.
	if err != nil {
		return nil, ErrUnavailable
	}
	return value, nil
}

func (s *Service) view(def Definition, row record) (View, error) {
	value, err := s.plain(def, row)
	if err != nil {
		return View{}, err
	}
	result := View{Key: def.Key, Description: def.Description, Secret: def.Secret, Configured: true,
		Revision: row.Revision, UpdatedAt: row.UpdatedAt}
	if def.Secret {
		// Presence is the only readable secret metadata. In particular, neither
		// ciphertext length nor a fingerprint is exposed as an additional oracle.
		var secret string
		_ = json.Unmarshal(value, &secret) // Registration guarantees a JSON string.
		result.Configured = secret != ""
	} else {
		result.Value = value
	}
	return result, nil
}

func (s *Service) List(ctx context.Context) ([]View, error) {
	// Query only registered keys. Removed definitions may remain in backups,
	// but they must not accidentally become an unregistered configuration API.
	result := make([]View, 0, len(s.keys))
	if len(s.keys) == 0 {
		return result, nil
	}
	var rows []record
	keys := make([]any, len(s.keys))
	for i, key := range s.keys {
		keys[i] = key
	}
	// key 是 MySQL 保留字，交给 GORM 的列表达式生成对应方言引号
	// 这里不能手写 PostgreSQL 双引号，也不能拼接 MySQL 反引号
	if err := s.db.WithContext(ctx).Where(clause.IN{Column: "key", Values: keys}).
		Order(clause.OrderByColumn{Column: clause.Column{Name: "key"}}).Find(&rows).Error; err != nil || len(rows) != len(s.keys) {
		// A missing registered row indicates incomplete initialization. Returning
		// a partial list would make a broken installation look like valid state.
		return nil, ErrUnavailable
	}
	for _, row := range rows {
		view, err := s.view(s.definitions[row.Key], row)
		if err != nil {
			return nil, err
		}
		result = append(result, view)
	}
	return result, nil
}

func (s *Service) read(ctx context.Context, key string) (Definition, record, error) {
	// Unknown keys are rejected before any SQL is issued; this is an allowlist
	// service rather than a general-purpose key-value store for HTTP callers.
	def, exists := s.definitions[key]
	if !exists {
		return Definition{}, record{}, ErrNotFound
	}
	var row record
	if err := s.db.WithContext(ctx).Where(clause.Eq{Column: "key", Value: key}).First(&row).Error; err != nil {
		return Definition{}, record{}, ErrUnavailable
	}
	return def, row, nil
}

func (s *Service) Get(ctx context.Context, key string) (View, error) {
	// Even a redacted read authenticates the stored ciphertext. A configured
	// badge must not claim a tampered or undecryptable secret is operational.
	def, row, err := s.read(ctx, key)
	if err != nil {
		return View{}, err
	}
	return s.view(def, row)
}

// Value is the trusted in-process accessor. HTTP handlers must use Get/List;
// exposing this method's result would bypass the write-only secret contract.
func (s *Service) Value(ctx context.Context, key string) (json.RawMessage, error) {
	def, row, err := s.read(ctx, key)
	if err != nil {
		return nil, err
	}
	return s.plain(def, row)
}

func (s *Service) Update(ctx context.Context, key string, value json.RawMessage, expectedRevision uint64, actor string) (View, error) {
	// Validation and encryption happen before opening the short transaction.
	// No network calls or domain callbacks run while the database holds row locks.
	def, exists := s.definitions[key]
	if !exists {
		return View{}, ErrNotFound
	}
	if expectedRevision == 0 || expectedRevision >= math.MaxInt64 || !validActor(actor) {
		// PostgreSQL bigint is signed; use the same range on both drivers instead
		// of allowing an overflow or a special value that bypasses concurrency.
		return View{}, ErrInvalidInput
	}
	canonical, err := validate(def, value, true)
	if err != nil {
		return View{}, err
	}
	payload := []byte(canonical)
	if def.Secret {
		payload, err = s.seal(key, payload)
		if err != nil {
			return View{}, err
		}
	}
	var updated record
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// The revision predicate makes this a compare-and-swap operation.
		// Two concurrent writers cannot both overwrite the same revision.
		// Use the same timestamp for the value and its audit so exports can
		// correlate one atomic change without comparing independently set times.
		now := time.Now().UTC()
		result := tx.Model(&record{}).Where(clause.Eq{Column: "key", Value: key}).
			Where("revision = ? AND secret = ?", expectedRevision, def.Secret).
			Updates(map[string]any{"payload": payload, "revision": expectedRevision + 1, "updated_at": now})
		if result.Error != nil {
			return ErrUnavailable
		}
		if result.RowsAffected != 1 {
			var current record
			if err := tx.Where(clause.Eq{Column: "key", Value: key}).First(&current).Error; err != nil || current.Secret != def.Secret {
				return ErrUnavailable
			}
			return ErrConflict
		}
		// Audit insertion shares the transaction: an audit failure rolls back
		// the value and revision, so an accepted write is always traceable.
		audit := Audit{Key: key, Actor: actor, OldRevision: expectedRevision, NewRevision: expectedRevision + 1, CreatedAt: now}
		if err := tx.Create(&audit).Error; err != nil {
			return ErrUnavailable
		}
		if err := tx.Where(clause.Eq{Column: "key", Value: key}).First(&updated).Error; err != nil {
			return ErrUnavailable
		}
		return nil
	})
	if err != nil {
		return View{}, storageError(err)
	}
	return s.view(def, updated)
}

func (s *Service) Audit(ctx context.Context, limit, offset int) ([]Audit, int64, error) {
	// Administrative history is bounded per request. Count and page are a
	// current observation rather than a snapshot across concurrent new writes;
	// audit correctness comes from the write transaction, not pagination timing.
	if limit < 1 || limit > 100 || offset < 0 {
		return nil, 0, ErrInvalidInput
	}
	var total int64
	query := s.db.WithContext(ctx).Model(&Audit{})
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, ErrUnavailable
	}
	rows := make([]Audit, 0)
	if err := query.Order("id DESC").Limit(limit).Offset(offset).Find(&rows).Error; err != nil {
		return nil, 0, ErrUnavailable
	}
	return rows, total, nil
}

func storageError(err error) error {
	if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) || errors.Is(err, ErrInvalidInput) || errors.Is(err, ErrUnavailable) {
		return err
	}
	// Database errors may contain SQL parameters, DSNs or internal schema
	// names. The transport receives only stable sentinel errors for mapping.
	return ErrUnavailable
}

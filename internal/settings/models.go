// Package settings stores registered runtime configuration independently of HTTP.
// Bootstrap database credentials and the encryption master key stay in files so a
// database backup cannot unlock its own encrypted settings.
package settings

import (
	"encoding/json"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// Stable sentinels let the transport map status codes without parsing strings.
// Validation details and driver errors are intentionally absent: either may
// contain a submitted secret even when the failure itself is harmless.
var (
	ErrNotFound     = errors.New("setting not found")
	ErrConflict     = errors.New("setting revision conflict")
	ErrInvalidInput = errors.New("invalid setting input")
	ErrUnavailable  = errors.New("settings unavailable")
)

// Definition is the allowlist and type contract for one namespaced setting.
// Defaults only seed a missing row; changing a default never overwrites an
// administrator's persisted value. Validate must inspect values without logging
// them, because it also receives decrypted secret values during initialization.
// The default fixes the top-level JSON type. Object/array definitions should
// additionally validate their nested schema in Validate. Secret definitions must
// use strings and should allow an empty string so administrators can clear them.
type Definition struct {
	Key         string
	Description string
	Default     json.RawMessage
	Secret      bool
	Validate    func(json.RawMessage) error
}

// View deliberately has no encrypted payload field. A secret's Value is always
// absent, including immediately after an update; clients can only see its state.
type View struct {
	Key         string          `json:"key"`
	Description string          `json:"description"`
	Value       json.RawMessage `json:"value,omitempty"`
	Secret      bool            `json:"secret"`
	Configured  bool            `json:"configured"`
	Revision    uint64          `json:"revision"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// Audit records who changed a setting and which revision they replaced.
// Neither old/new values nor ciphertext belong in this table or audit response.
type Audit struct {
	ID          uint64    `gorm:"primaryKey" json:"id"`
	Key         string    `gorm:"size:128;not null;index" json:"key"`
	Actor       string    `gorm:"size:128;not null" json:"actor"`
	OldRevision uint64    `gorm:"not null" json:"old_revision"`
	NewRevision uint64    `gorm:"not null" json:"new_revision"`
	CreatedAt   time.Time `gorm:"not null;index" json:"created_at"`
}

func (Audit) TableName() string { return "sys_setting_audits" }

// The persistence type is private to prevent controllers from accidentally
// serializing payloads. The binary column holds JSON bytes or authenticated
// ciphertext, distinguished by the immutable registered Secret attribute.
type record struct {
	Key       string        `gorm:"primaryKey;size:128"`
	Payload   binaryPayload `gorm:"not null"`
	Secret    bool          `gorm:"not null"`
	Revision  uint64        `gorm:"not null"`
	CreatedAt time.Time     `gorm:"not null"`
	UpdatedAt time.Time     `gorm:"not null"`
}

func (record) TableName() string { return "sys_settings" }

// binaryPayload preserves the existing bytes on PostgreSQL and chooses a large
// enough MySQL column for a 64 KiB JSON value plus the AES-GCM nonce and tag.
// GORM can scan a named byte slice directly; no encoding or value conversion is
// involved, so an existing PostgreSQL backup keeps the same ciphertext format.
type binaryPayload []byte

func (binaryPayload) GormDataType() string { return "bytes" }

func (binaryPayload) GormDBDataType(db *gorm.DB, _ *schema.Field) string {
	if db.Dialector.Name() == "mysql" {
		return "longblob"
	}
	return "bytea"
}

// Models lets the bootstrap layer own GORM schema initialization. Registering
// models does not open a database connection or perform migrations as a side effect.
// Keeping the list here ensures runtime registration and model initialization can
// evolve together without a transport package importing private persistence types.
func Models() []any { return []any{&record{}, &Audit{}} }

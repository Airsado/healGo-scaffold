package settings

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func testKey(fill byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))
}

func testDefinitions() []Definition {
	return []Definition{
		{Key: "system.site_name", Description: "Site name", Default: json.RawMessage(`"Original"`), Validate: func(value json.RawMessage) error {
			var text string
			if json.Unmarshal(value, &text) != nil || strings.TrimSpace(text) == "" || len(text) > 120 {
				return errors.New("invalid site name")
			}
			return nil
		}},
		{Key: "system.maintenance", Default: json.RawMessage(`false`)},
		{Key: "transport.api_key", Secret: true, Default: json.RawMessage(`""`), Validate: func(value json.RawMessage) error {
			if bytes.Contains(value, []byte("reject")) {
				return fmt.Errorf("validator deliberately includes sensitive input: %s", value)
			}
			return nil
		}},
	}
}

func initializedService(t *testing.T) (*Service, *gorm.DB) {
	t.Helper()
	db := testDatabase(t)
	service, err := New(db, testKey(1), testDefinitions())
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	return service, db
}

func TestInitializePreservesPersistedValuesAndTypes(t *testing.T) {
	service, db := initializedService(t)
	ctx := context.Background()
	updated, err := service.Update(ctx, "system.site_name", json.RawMessage(`"Saved"`), 1, "admin:1")
	if err != nil || updated.Revision != 2 || string(updated.Value) != `"Saved"` {
		t.Fatalf("save setting: %#v %v", updated, err)
	}
	definitions := testDefinitions()
	definitions[0].Default = json.RawMessage(`"Changed default"`)
	restarted, err := New(db, testKey(1), definitions)
	if err != nil || restarted.Initialize(ctx) != nil {
		t.Fatal("restart service failed")
	}
	view, err := restarted.Get(ctx, "system.site_name")
	if err != nil || string(view.Value) != `"Saved"` || view.Revision != 2 {
		t.Fatalf("persisted value was replaced by default: %#v %v", view, err)
	}
	for _, value := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(`true`), json.RawMessage(`"  "`), json.RawMessage(`"a" "b"`)} {
		if _, err := service.Update(ctx, "system.site_name", value, 2, "admin:1"); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid value accepted: %v", err)
		}
	}
	if _, err := service.Get(ctx, "unknown.key"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unregistered key read: %v", err)
	}
	if _, err := service.Update(ctx, "unknown.key", json.RawMessage(`"new"`), 1, "admin:1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unregistered key created: %v", err)
	}
	rows, total, err := service.Audit(ctx, 10, 0)
	if err != nil || total != 1 || len(rows) != 1 || rows[0].OldRevision != 1 || rows[0].NewRevision != 2 {
		t.Fatalf("audit records: %#v %d %v", rows, total, err)
	}
}

func TestSecretEncryptionRedactionClearAndRestart(t *testing.T) {
	service, db := initializedService(t)
	ctx := context.Background()
	// The empty default must already be encrypted and authenticate the key.
	wrongKey, _ := New(db, testKey(2), testDefinitions())
	if err := wrongKey.Initialize(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("wrong master accepted for empty secret: %v", err)
	}
	const secret = `"test-top-secret-7f3e92a6"`
	view, err := service.Update(ctx, "transport.api_key", json.RawMessage(secret), 1, "admin:1")
	if err != nil || !view.Configured || len(view.Value) != 0 || view.Revision != 2 {
		t.Fatalf("secret update metadata: %#v %v", view, err)
	}
	encodedView, _ := json.Marshal(view)
	if bytes.Contains(encodedView, []byte("test-top-secret")) || bytes.Contains(encodedView, []byte(`"value"`)) {
		t.Fatal("secret leaked in update response")
	}
	var stored record
	if err := db.Where(clause.Eq{Column: "key", Value: "transport.api_key"}).First(&stored).Error; err != nil {
		t.Fatal("read ciphertext failed")
	}
	firstCiphertext := append([]byte(nil), stored.Payload...)
	if bytes.Contains(stored.Payload, []byte("test-top-secret")) || bytes.Equal(stored.Payload, []byte(secret)) {
		t.Fatal("database contains plaintext secret")
	}
	restarted, _ := New(db, testKey(1), testDefinitions())
	if err := restarted.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	plain, err := restarted.Value(ctx, "transport.api_key")
	if err != nil || string(plain) != secret {
		t.Fatal("secret did not survive restart")
	}
	views, err := restarted.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range views {
		if item.Secret && (len(item.Value) != 0 || !item.Configured) {
			t.Fatal("secret list state is not redacted")
		}
	}
	if _, err := restarted.Update(ctx, "transport.api_key", json.RawMessage(secret), 2, "admin:1"); err != nil {
		t.Fatal(err)
	}
	if err := db.Where(clause.Eq{Column: "key", Value: "transport.api_key"}).First(&stored).Error; err != nil || bytes.Equal(firstCiphertext, stored.Payload) {
		t.Fatal("same secret write reused ciphertext nonce")
	}
	view, err = restarted.Update(ctx, "transport.api_key", json.RawMessage(`null`), 3, "admin:1")
	if err != nil || view.Configured || len(view.Value) != 0 || view.Revision != 4 {
		t.Fatalf("explicit secret clear failed: %#v %v", view, err)
	}
	plain, err = restarted.Value(ctx, "transport.api_key")
	if err != nil || string(plain) != `""` {
		t.Fatal("clear did not persist empty secret")
	}
	if _, err := restarted.Update(ctx, "transport.api_key", json.RawMessage(`"reject-sensitive"`), 4, "admin:1"); !errors.Is(err, ErrInvalidInput) || strings.Contains(err.Error(), "sensitive") {
		t.Fatal("validator reflected sensitive data")
	}
	audits, _, err := restarted.Audit(ctx, 100, 0)
	encodedAudit, _ := json.Marshal(audits)
	if err != nil || bytes.Contains(encodedAudit, []byte("test-top-secret")) || bytes.Contains(encodedAudit, []byte(`"value"`)) {
		t.Fatal("secret leaked in audit")
	}
}

func TestConcurrentRevisionOneWinsAndAuditRollsBack(t *testing.T) {
	service, db := initializedService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for _, value := range []string{`"Writer A"`, `"Writer B"`} {
		wait.Add(1)
		go func(value string) {
			defer wait.Done()
			<-start
			_, err := service.Update(ctx, "system.site_name", json.RawMessage(value), 1, "admin:1")
			results <- err
		}(value)
	}
	close(start)
	wait.Wait()
	close(results)
	var successes, conflicts int
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatalf("unexpected concurrent result: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("CAS successes=%d conflicts=%d", successes, conflicts)
	}
	before, _ := service.Get(ctx, "system.site_name")
	_, count, _ := service.Audit(ctx, 10, 0)
	if count != 1 {
		t.Fatalf("concurrent audit count=%d", count)
	}
	// Force the audit insertion to fail at the database boundary. This tests
	// atomic rollback rather than merely assuming Transaction did the right thing.
	if err := db.Exec(`ALTER TABLE sys_setting_audits ADD CONSTRAINT reject_actor CHECK (actor <> 'blocked')`).Error; err != nil {
		t.Fatal("install test audit constraint failed")
	}
	if _, err := service.Update(ctx, "system.site_name", json.RawMessage(`"Must roll back"`), before.Revision, "blocked"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("audit failure accepted: %v", err)
	}
	after, _ := service.Get(ctx, "system.site_name")
	_, count, _ = service.Audit(ctx, 10, 0)
	if after.Revision != before.Revision || !bytes.Equal(after.Value, before.Value) || count != 1 {
		t.Fatal("audit failure did not roll back value and revision")
	}
}

func TestTamperAndCrossKeyCiphertextFailClosed(t *testing.T) {
	service, db := initializedService(t)
	ctx := context.Background()
	var original record
	if err := db.Where(clause.Eq{Column: "key", Value: "transport.api_key"}).First(&original).Error; err != nil {
		t.Fatal("read test record failed")
	}
	tampered := append([]byte(nil), original.Payload...)
	tampered[len(tampered)-1] ^= 0x01
	if err := db.Model(&record{}).Where(clause.Eq{Column: "key", Value: original.Key}).Update("payload", tampered).Error; err != nil {
		t.Fatal("tamper setup failed")
	}
	if err := service.Initialize(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatal("tampered secret accepted on startup")
	}
	if _, err := service.Get(ctx, original.Key); !errors.Is(err, ErrUnavailable) {
		t.Fatal("tampered secret accepted by redacted read")
	}
	if _, err := service.open("other.api_key", original.Payload); !errors.Is(err, ErrUnavailable) {
		t.Fatal("ciphertext copied to another key was accepted")
	}
}

func TestRegistrationAndValidation(t *testing.T) {
	// Validation tests have no database side effects and run without either database.
	for _, definitions := range [][]Definition{
		{{Key: "not_namespaced", Default: json.RawMessage(`true`)}},
		{{Key: "system.null", Default: json.RawMessage(`null`)}},
		{{Key: "system.bad", Default: json.RawMessage(`{`)}},
		{{Key: "system.secret", Default: json.RawMessage(`true`), Secret: true}},
		{{Key: "system.same", Default: json.RawMessage(`true`)}, {Key: "system.same", Default: json.RawMessage(`false`)}},
	} {
		if _, err := New(&gorm.DB{}, testKey(1), definitions); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid definition accepted: %v", err)
		}
	}
	for _, key := range []string{"", "short", base64.StdEncoding.EncodeToString(make([]byte, 31))} {
		if _, err := New(&gorm.DB{}, key, testDefinitions()); !errors.Is(err, ErrInvalidInput) {
			t.Fatal("invalid master key accepted")
		}
	}
	largeInteger := json.RawMessage(`9007199254740993`)
	canonical, err := normalized(largeInteger)
	if err != nil || !bytes.Equal(canonical, largeInteger) {
		t.Fatal("generic JSON normalization rounded integer")
	}
}

func TestConcurrentInitializationAndMaximumEncryptedPayload(t *testing.T) {
	database := testDatabase(t)
	service, err := New(database, testKey(1), testDefinitions())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			results <- service.Initialize(ctx)
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal("concurrent settings initialization failed", err)
		}
	}
	// The valid JSON string itself fills the 64 KiB budget. Encryption adds
	// nonce and authentication bytes, which exceed MySQL's ordinary BLOB size.
	value, err := json.Marshal(strings.Repeat("x", maxValueBytes-2))
	if err != nil || len(value) != maxValueBytes {
		t.Fatal("construct maximum-sized JSON value")
	}
	view, err := service.Update(ctx, "transport.api_key", value, 1, "admin:1")
	if err != nil || view.Revision != 2 || !view.Configured || len(view.Value) != 0 {
		t.Fatal("maximum encrypted payload was not persisted safely", err)
	}
	actual, err := service.Value(ctx, "transport.api_key")
	if err != nil || !bytes.Equal(actual, value) {
		t.Fatal("maximum encrypted payload did not round-trip", err)
	}
	// A second migration and initialize must preserve both column capacity and
	// ciphertext bytes; restarting must never replace persisted values with seeds.
	if err := database.AutoMigrate(Models()...); err != nil {
		t.Fatal("repeat settings schema migration failed")
	}
	if err := service.Initialize(ctx); err != nil {
		t.Fatal("restart with maximum payload failed", err)
	}
	actual, err = service.Value(ctx, "transport.api_key")
	if err != nil || !bytes.Equal(actual, value) {
		t.Fatal("restart replaced maximum encrypted payload", err)
	}
}

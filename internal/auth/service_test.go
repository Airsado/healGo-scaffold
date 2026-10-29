package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"heago-scaffold/configs"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

func serviceForTest(t *testing.T, db *gorm.DB, ttl time.Duration) *Service {
	t.Helper()
	s, err := New(db, configs.Auth{SessionTTL: ttl, MaxSessions: 5})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Bootstrap(context.Background(), "admin", "correct-password-1"); err != nil {
		t.Fatal(err)
	}
	return s
}

func authenticateToken(s *Service, token string) (Principal, error) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	p, _, err := s.Authenticate(r.Context(), r)
	return p, err
}

func TestValidationAndSecretSerialization(t *testing.T) {
	if cost, err := bcrypt.Cost([]byte(dummyHash)); err != nil || cost != bcrypt.DefaultCost {
		t.Fatal("dummy hash must remain a real default-cost bcrypt hash")
	}
	for _, input := range []string{"a", "admin?", ".admin", "has space", strings.Repeat("x", 65)} {
		if validName(input) {
			t.Fatalf("invalid identity name accepted: %q", input)
		}
	}
	for _, input := range []string{"", "short", strings.Repeat("x", 73), strings.Repeat(" ", 12)} {
		if validPassword(input) {
			t.Fatal("invalid password accepted")
		}
	}
	if !validPassword(strings.Repeat("x", 72)) {
		t.Fatal("bcrypt maximum rejected")
	}
	for _, input := range []string{"", "a::b", "a:*b", "has space", ":read", strings.Repeat("x", 129)} {
		if validPermission(input) {
			t.Fatalf("invalid permission accepted: %q", input)
		}
	}
	for _, input := range []string{"*", "system:user:read", "system:*", "system:*:read"} {
		if !validPermission(input) {
			t.Fatalf("valid permission rejected: %q", input)
		}
	}
	userJSON, _ := json.Marshal(User{ID: 1, PasswordHash: "never-serialize-this", AuthVersion: 99})
	if strings.Contains(string(userJSON), "never-serialize-this") || strings.Contains(string(userJSON), "auth_version") {
		t.Fatal("private identity fields leaked through JSON")
	}
	principalJSON, _ := json.Marshal(Principal{ID: 9007199254740993, authVersion: 99})
	if !strings.Contains(string(principalJSON), `"id":"9007199254740993"`) || strings.Contains(string(principalJSON), "authVersion") {
		t.Fatal("principal ID precision or version privacy regressed")
	}
}

func TestBootstrapPersistenceAndHeaderOnlyAuthentication(t *testing.T) {
	ctx := context.Background()
	db := isolatedDB(t)
	s := serviceForTest(t, db, time.Hour)
	if _, _, err := s.Login(ctx, "admin", "incorrect-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal("wrong password was not rejected")
	}
	if _, _, err := s.Login(ctx, "unknown", "correct-password-1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal("unknown account error reveals its existence")
	}
	token, principal, err := s.Login(ctx, "admin", "correct-password-1")
	if err != nil || principal.Username != "admin" || !contains(principal.Permissions, "*") {
		t.Fatal("initial administrator login failed", err)
	}
	for _, tc := range []struct {
		name, header, url, cookie string
		duplicate                 bool
		valid                     bool
	}{
		{name: "bearer", header: "Bearer " + token, url: "/", valid: true},
		{name: "bare", header: token, url: "/"},
		{name: "query", url: "/?Authorization=" + token},
		{name: "cookie", url: "/", cookie: "Authorization=" + token},
		{name: "extra space", header: "Bearer  " + token, url: "/"},
		{name: "merged", header: "Bearer " + token + ",Bearer " + token, url: "/"},
		{name: "duplicate", header: "Bearer " + token, url: "/", duplicate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", tc.url, nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			if tc.duplicate {
				r.Header.Add("Authorization", tc.header)
			}
			if tc.cookie != "" {
				r.Header.Set("Cookie", tc.cookie)
			}
			p, _, err := s.Authenticate(ctx, r)
			if tc.valid && (err != nil || p.ID != principal.ID) {
				t.Fatal("valid token rejected", err)
			}
			if !tc.valid && !errors.Is(err, ErrUnauthorized) {
				t.Fatal("ambiguous credential accepted", err)
			}
		})
	}
	if err := s.RequirePermission(ctx, principal, "system:users:write"); err != nil {
		t.Fatal("Sa-Token administrator wildcard failed", err)
	}
	if err := s.RequireRole(ctx, principal, "admin"); err != nil {
		t.Fatal("Sa-Token role check failed", err)
	}
	// A second seed must never rotate an existing password or add an account.
	if err := s.Bootstrap(ctx, "different-admin", "different-password-1"); err != nil {
		t.Fatal(err)
	}
	users, total, err := s.ListUsers(ctx, 100, 0)
	if err != nil || total != 1 || len(users) != 1 || users[0].Username != "admin" {
		t.Fatal("bootstrap unexpectedly overwrote persisted identity", err)
	}
	restarted := serviceForTest(t, db, time.Hour)
	if _, err := authenticateToken(restarted, token); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("memory sessions survived process manager restart")
	}
	if _, _, err := restarted.Login(ctx, "admin", "correct-password-1"); err != nil {
		t.Fatal("persisted password unavailable after restart", err)
	}
	if err := s.Logout(token); err != nil {
		t.Fatal(err)
	}
	if _, err := authenticateToken(s, token); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("logged-out token remained valid")
	}
}

func TestLiveRolesPasswordAndDisableRevocation(t *testing.T) {
	ctx := context.Background()
	db := isolatedDB(t)
	s := serviceForTest(t, db, time.Hour)
	_, admin, err := s.Login(ctx, "admin", "correct-password-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutRole(ctx, "viewer", []string{"system:settings:read"}); err != nil {
		t.Fatal(err)
	}
	viewer, err := s.CreateUser(ctx, "alice", "viewer-password-1", []string{"viewer"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, "alice", "viewer-password-2", nil); !errors.Is(err, ErrConflict) {
		t.Fatal("duplicate account was not rejected atomically", err)
	}
	if _, err := s.CreateUser(ctx, "broken", "viewer-password-2", []string{"missing"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("undefined role accepted", err)
	}
	_, total, err := s.ListUsers(ctx, 100, 0)
	if err != nil || total != 2 {
		t.Fatal("failed user creation was not rolled back", total, err)
	}
	token, viewer, err := s.Login(ctx, "alice", "viewer-password-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RequirePermission(ctx, viewer, "system:settings:read"); err != nil {
		t.Fatal("viewer permission not granted", err)
	}
	if err := s.RequirePermission(ctx, viewer, "system:users:write"); !errors.Is(err, ErrForbidden) {
		t.Fatal("viewer received administrator access", err)
	}
	if err := s.RequireRole(ctx, viewer, "admin"); !errors.Is(err, ErrForbidden) {
		t.Fatal("viewer passed administrator role check", err)
	}
	if err := s.PutRole(ctx, "viewer", []string{"system:users:read"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RequirePermission(ctx, viewer, "system:settings:read"); !errors.Is(err, ErrForbidden) {
		t.Fatal("old in-memory permission survived persisted role update", err)
	}
	if err := s.RequirePermission(ctx, viewer, "system:users:read"); err != nil {
		t.Fatal("new role permission not applied to existing token", err)
	}
	if err := s.SetUserRoles(ctx, viewer.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.RequirePermission(ctx, viewer, "system:users:read"); !errors.Is(err, ErrForbidden) {
		t.Fatal("removed role left stale account-session access", err)
	}
	if err := s.ChangePassword(ctx, viewer.ID, "incorrect-password", "replacement-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal("password changed without proving current password", err)
	}
	if err := s.ChangePassword(ctx, viewer.ID, "viewer-password-1", "replacement-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := authenticateToken(s, token); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("password change did not invalidate old session", err)
	}
	if _, _, err := s.Login(ctx, "alice", "viewer-password-1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal("old password remains valid", err)
	}
	token, _, err = s.Login(ctx, "alice", "replacement-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetEnabled(ctx, viewer.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := authenticateToken(s, token); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("disabled account token remained valid", err)
	}
	if _, _, err := s.Login(ctx, "alice", "replacement-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal("disabled account logged in", err)
	}
	if err := s.SetEnabled(ctx, viewer.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := authenticateToken(s, token); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("reenabling revived a token issued before disabling", err)
	}
	if err := s.SetEnabled(ctx, admin.ID, false); !errors.Is(err, ErrForbidden) {
		t.Fatal("last administrator disabled", err)
	}
	if err := s.SetUserRoles(ctx, admin.ID, []string{"viewer"}); !errors.Is(err, ErrForbidden) {
		t.Fatal("last administrator role removed", err)
	}
	if err := s.PutRole(ctx, "admin", []string{"system:users:read"}); !errors.Is(err, ErrForbidden) {
		t.Fatal("administrator recovery permission removed", err)
	}
	if err := s.RequirePermission(ctx, admin, "system:users:write"); err != nil {
		t.Fatal("rejected mutations were not rolled back", err)
	}
}

func TestConcurrentRBACChecksAndLastAdminInvariant(t *testing.T) {
	ctx := context.Background()
	s := serviceForTest(t, isolatedDB(t), time.Hour)
	_, admin, err := s.Login(ctx, "admin", "correct-password-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutRole(ctx, "viewer", []string{"system:settings:read"}); err != nil {
		t.Fatal(err)
	}
	viewer, err := s.CreateUser(ctx, "viewer", "viewer-password-1", []string{"viewer"})
	if err != nil {
		t.Fatal(err)
	}
	_, viewer, err = s.Login(ctx, "viewer", "viewer-password-1")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := s.RequirePermission(ctx, admin, "system:users:write"); err != nil {
				t.Error("admin concurrent permission", err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := s.RequirePermission(ctx, viewer, "system:users:write"); !errors.Is(err, ErrForbidden) {
				t.Error("viewer concurrent privilege isolation", err)
			}
		}()
	}
	wg.Wait()
	second, err := s.CreateUser(ctx, "second-admin", "second-password-1", []string{"admin"})
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for _, id := range []uint64{admin.ID, second.ID} {
		wg.Add(1)
		go func(id uint64) {
			defer wg.Done()
			results <- s.SetEnabled(ctx, id, false)
		}(id)
	}
	wg.Wait()
	close(results)
	var succeeded, rejected int
	for err := range results {
		if err == nil {
			succeeded++
		} else if errors.Is(err, ErrForbidden) {
			rejected++
		} else {
			t.Fatal(err)
		}
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatal("concurrent mutations lost the last-admin invariant", succeeded, rejected)
	}
}

func TestSessionExpirationAndBoundedConcurrentLogin(t *testing.T) {
	ctx := context.Background()
	db := isolatedDB(t)
	s := serviceForTest(t, db, time.Hour)
	var tokens []string
	for i := 0; i < 6; i++ {
		token, _, err := s.Login(ctx, "admin", "correct-password-1")
		if err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, token)
	}
	var active int
	for _, token := range tokens {
		if _, err := authenticateToken(s, token); err == nil {
			active++
		} else if !errors.Is(err, ErrUnauthorized) {
			t.Fatal("unexpected session error", err)
		}
	}
	if active != 5 {
		t.Fatal("session overflow did not enforce configured bound", active)
	}
	if _, err := authenticateToken(s, tokens[len(tokens)-1]); err != nil {
		t.Fatal("latest token rejected", err)
	}
	expiring := serviceForTest(t, db, time.Second)
	token, _, err := expiring.Login(ctx, "admin", "correct-password-1")
	if err != nil {
		t.Fatal(err)
	}
	// The library stores seconds, so wait across two boundaries to avoid flakes.
	time.Sleep(2100 * time.Millisecond)
	if _, err := authenticateToken(expiring, token); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("expired token remained authenticated")
	}
}

func TestConcurrentBootstrapAndCaseSensitiveNames(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	database := isolatedDB(t)
	services := make([]*Service, 2)
	for index := range services {
		service, err := New(database, configs.Auth{SessionTTL: time.Hour, MaxSessions: 5})
		if err != nil {
			t.Fatal(err)
		}
		services[index] = service
		t.Cleanup(func() { _ = service.Close() })
	}
	// Two independent services model concurrent application startup. The lock
	// must live in the database, otherwise both can observe an empty user table.
	start := make(chan struct{})
	results := make(chan error, len(services))
	for _, service := range services {
		go func(service *Service) {
			<-start
			results <- service.Bootstrap(ctx, "admin", "correct-password-1")
		}(service)
	}
	close(start)
	for range services {
		if err := <-results; err != nil {
			t.Fatal("concurrent bootstrap failed", err)
		}
	}
	var users, roles, assignments int64
	if database.Model(&User{}).Count(&users).Error != nil || database.Model(&Role{}).Count(&roles).Error != nil ||
		database.Model(&UserRole{}).Count(&assignments).Error != nil || users != 1 || roles != 1 || assignments != 1 {
		t.Fatal("concurrent bootstrap produced incomplete or duplicate identities")
	}
	// MySQL's default collation is usually case-insensitive. The test database
	// and application database must use binary collation to match PostgreSQL.
	service := services[0]
	for role, permission := range map[string]string{"Viewer": "case:upper:read", "viewer": "case:lower:read"} {
		if err := service.PutRole(ctx, role, []string{permission}); err != nil {
			t.Fatal("case-distinct role failed", err)
		}
	}
	upper, err := service.CreateUser(ctx, "Alice", "case-password-1", []string{"Viewer"})
	if err != nil {
		t.Fatal("create upper-case identity", err)
	}
	lower, err := service.CreateUser(ctx, "alice", "case-password-2", []string{"viewer"})
	if err != nil || upper.ID == lower.ID {
		t.Fatal("case-distinct identities collapsed", err)
	}
	_, upper, err = service.Login(ctx, "Alice", "case-password-1")
	if err != nil || !contains(upper.Roles, "Viewer") || !contains(upper.Permissions, "case:upper:read") || contains(upper.Permissions, "case:lower:read") {
		t.Fatal("upper-case identity received wrong role", err)
	}
	_, lower, err = service.Login(ctx, "alice", "case-password-2")
	if err != nil || !contains(lower.Roles, "viewer") || !contains(lower.Permissions, "case:lower:read") || contains(lower.Permissions, "case:upper:read") {
		t.Fatal("lower-case identity received wrong role", err)
	}
}

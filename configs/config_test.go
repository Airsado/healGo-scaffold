package configs

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validConfig() Config {
	c := defaults()
	c.Database.Password = "private-postgres-credential"
	c.Bootstrap.AdminPassword = "private-admin-credential"
	c.Security.EncryptionKey = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	return c
}

func minimalYAML() string {
	return "database:\n  password: private-postgres-credential\nbootstrap:\n  admin_password: private-admin-credential\nsecurity:\n  encryption_key: " + validConfig().Security.EncryptionKey + "\n"
}

func loadYAML(t *testing.T, text string) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestLoadDefaultsAndStrictDocuments(t *testing.T) {
	cfg, err := loadYAML(t, minimalYAML())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.RequestTimeout != 30*time.Second || cfg.Database.AutoMigrate || cfg.Auth.LoginAttemptsPerMinute != 60 {
		t.Fatalf("unexpected safe defaults")
	}
	for _, suffix := range []string{"unknown_field: ignored\n", "---\n{}\n", "---\n", "http:\n  request_timeuot: 10s\n"} {
		if _, err := loadYAML(t, minimalYAML()+suffix); err == nil {
			t.Fatalf("accepted invalid document suffix %q", suffix)
		}
	}
}

func TestErrorsAndFormattingDoNotRevealSecrets(t *testing.T) {
	secret := "never-print-this-private-value"
	_, err := loadYAML(t, minimalYAML()+"http:\n  read_timeout: "+secret+"\n")
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("parser leaked a value: %v", err)
	}
	c := validConfig()
	for _, formatted := range []string{fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c), fmt.Sprintf("%#v", c.Database), fmt.Sprint(c.Database), fmt.Sprint(c.Bootstrap), fmt.Sprint(c.Security)} {
		if strings.Contains(formatted, c.Database.Password) || strings.Contains(formatted, c.Bootstrap.AdminPassword) || strings.Contains(formatted, c.Security.EncryptionKey) {
			t.Fatal("formatted config leaked a secret")
		}
	}
}

func TestValidateSecurityBoundaries(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Config)
	}{
		{"placeholder database password", func(c *Config) { c.Database.Password = "REPLACE_WITH_DATABASE_PASSWORD" }},
		{"placeholder admin password", func(c *Config) { c.Bootstrap.AdminPassword = "change-me-before-starting" }},
		{"short encryption key", func(c *Config) { c.Security.EncryptionKey = base64.StdEncoding.EncodeToString([]byte("too-short")) }},
		{"invalid origin", func(c *Config) { c.Security.CORSOrigins = []string{"https://example.com/path"} }},
		{"wildcard origin", func(c *Config) { c.Security.CORSOrigins = []string{"https://*.example.com"} }},
		{"invalid proxy", func(c *Config) { c.Security.TrustedProxies = []string{"some-proxy"} }},
		{"unbounded connections", func(c *Config) { c.Database.MaxOpen = 0 }},
		{"idle exceeds pool", func(c *Config) { c.Database.MaxIdle = c.Database.MaxOpen + 1 }},
		{"timeout exceeds write", func(c *Config) { c.HTTP.RequestTimeout = c.HTTP.WriteTimeout }},
		{"unbounded retries", func(c *Config) { c.Database.RetryAttempts = 100 }},
		{"invalid HTTP port", func(c *Config) { c.HTTP.Addr = ":0" }},
		{"fractional token TTL", func(c *Config) { c.Auth.SessionTTL = time.Minute + time.Millisecond }},
		{"punctuation-leading username", func(c *Config) { c.Bootstrap.AdminUsername = ".admin" }},
		{"invalid level", func(c *Config) { c.Logging.Level = "trace" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.change(&c)
			if err := Validate(c); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	valid := validConfig()
	valid.Security.TrustedProxies = []string{"127.0.0.1", "::1", "10.0.0.0/8"}
	valid.Security.CORSOrigins = []string{"https://admin.example.com", "http://localhost:5173"}
	if err := Validate(valid); err != nil {
		t.Fatal(err)
	}
}

func TestExampleRequiresPrivateConfiguration(t *testing.T) {
	for _, path := range []string{"config.example.yaml", "config.mysql.example.yaml", "config.replication.example.yaml"} {
		if _, err := Load(path); err == nil {
			t.Fatalf("placeholder example %s must not start a server", path)
		}
	}
}

func TestLoadRejectsOversizedAndEmptyInput(t *testing.T) {
	for _, text := range []string{"", strings.Repeat("a", (1<<20)+1)} {
		if _, err := loadYAML(t, text); err == nil {
			t.Fatal("invalid input was accepted")
		}
	}
}

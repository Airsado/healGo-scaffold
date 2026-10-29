// Package configs owns deployment bootstrap values, not editable business settings.
// Database credentials must exist before the application can read persisted settings.
package configs

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config separates immutable process bootstrap from settings persisted by services.
// Secret-bearing structures intentionally redact themselves when formatted for logs.
type Config struct {
	App       App       `yaml:"app"`
	HTTP      HTTP      `yaml:"http"`
	Database  Database  `yaml:"database"`
	Auth      Auth      `yaml:"auth"`
	Bootstrap Bootstrap `yaml:"bootstrap"`
	Security  Security  `yaml:"security"`
	Logging   Logging   `yaml:"logging"`
}

type App struct {
	// Name identifies the service in logs and health metadata, without becoming a routing prefix.
	Name string `yaml:"name"`
	// Environment selects runtime behavior; spelling mistakes must not accidentally enable debug mode.
	Environment string `yaml:"environment"`
}

// HTTP exposes server timeouts explicitly so a slow client cannot hold connections forever.
type HTTP struct {
	// Addr is a host:port pair. An empty host intentionally listens on all container interfaces.
	Addr string `yaml:"addr"`
	// Header timeout stops clients that send headers one byte at a time.
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	// Read timeout also bounds receiving a complete request body.
	ReadTimeout time.Duration `yaml:"read_timeout"`
	// Write timeout must exceed the request budget so the handler can send its timeout response.
	WriteTimeout time.Duration `yaml:"write_timeout"`
	// Idle timeout releases keep-alive connections that no longer send requests.
	IdleTimeout time.Duration `yaml:"idle_timeout"`
	// Shutdown timeout limits how long deployment waits for outstanding requests.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	// Request timeout propagates cancellation to services and database queries.
	RequestTimeout time.Duration `yaml:"request_timeout"`
}

// Auth uses a bounded memory store in this single-process template.
// A shared store must replace it before running several application replicas.
type Auth struct {
	// Whole seconds match Sa-Token's expiry unit and avoid silently truncating a duration.
	SessionTTL time.Duration `yaml:"session_ttl"`
	// A finite session count bounds retained credentials for the single administrator.
	MaxSessions int `yaml:"max_sessions"`
	// Login attempts are limited before password hashing, which is intentionally CPU-expensive.
	LoginAttemptsPerMinute int `yaml:"login_attempts_per_minute"`
}

// Bootstrap is used only when the administrator is absent from the database.
// Keeping the original password here is not a password-reset mechanism.
type Bootstrap struct {
	AdminUsername string `yaml:"admin_username"`
	AdminPassword string `yaml:"admin_password" json:"-"`
}

type Security struct {
	// The encryption key stays outside the database it protects; losing it prevents decryption.
	EncryptionKey string `yaml:"encryption_key" json:"-"`
	// Trust only known reverse proxies, otherwise clients can forge their forwarded address.
	TrustedProxies []string `yaml:"trusted_proxies"`
	// Exact origins make a browser's access decision explicit; wildcard credentials are forbidden.
	CORSOrigins []string `yaml:"cors_origins"`
	// A body limit prevents JSON decoding from allocating arbitrary amounts of memory.
	MaxBodyBytes int64 `yaml:"max_body_bytes"`
}

type Logging struct {
	// Level filters events before formatting; debug mode never enables SQL-value logging.
	Level string `yaml:"level"`
	// Format changes presentation only; log destinations remain platform controlled.
	Format string `yaml:"format"`
}

func (Config) String() string    { return "Config{bootstrap values redacted}" }
func (Database) String() string  { return "Database{connection values redacted}" }
func (Bootstrap) String() string { return "Bootstrap{credentials redacted}" }
func (Security) String() string  { return "Security{security values redacted}" }

// GoString also covers %#v, which otherwise bypasses fmt.Stringer during debugging.
func (c Config) GoString() string    { return c.String() }
func (c Database) GoString() string  { return c.String() }
func (c Bootstrap) GoString() string { return c.String() }
func (c Security) GoString() string  { return c.String() }

// Defaults are conservative for one process; production credentials have no defaults.
// AutoMigrate defaults to false: schema changes require an explicit operator decision.
func defaults() Config {
	return Config{
		App: App{Name: "healGo-scaffold", Environment: "development"},
		HTTP: HTTP{Addr: ":8080", ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
			WriteTimeout: 35 * time.Second, IdleTimeout: 60 * time.Second, ShutdownTimeout: 10 * time.Second,
			RequestTimeout: 30 * time.Second},
		Database: Database{Host: "127.0.0.1", Port: 5432, Name: "heago", User: "heago", SSLMode: "require", FallbackToPrimary: true,
			MaxOpen: 20, MaxIdle: 5, ConnMaxLifetime: 30 * time.Minute, ConnMaxIdleTime: 5 * time.Minute,
			ConnectTimeout: 5 * time.Second, RetryDelay: time.Second, RetryAttempts: 5},
		Auth:      Auth{SessionTTL: 24 * time.Hour, MaxSessions: 5, LoginAttemptsPerMinute: 60},
		Bootstrap: Bootstrap{AdminUsername: "admin"},
		Security:  Security{TrustedProxies: []string{}, CORSOrigins: []string{}, MaxBodyBytes: 1 << 20},
		Logging:   Logging{Level: "info", Format: "json"},
	}
}

// Load accepts exactly one YAML mapping. Strict keys turn misspelled security settings
// into startup failures, while fixed parser errors keep secret scalar values private.
func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, errors.New("cannot open configuration file")
	}
	defer f.Close()
	// Bound input before YAML parsing so accidental huge files cannot exhaust startup memory.
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return Config{}, errors.New("cannot read configuration file")
	}
	if len(data) > 1<<20 {
		return Config{}, errors.New("configuration file exceeds 1 MiB")
	}
	if strings.TrimSpace(string(data)) == "" {
		return Config{}, errors.New("configuration file is empty")
	}
	cfg := defaults()
	// Decode onto defaults: omitted operational limits remain safe, while secrets remain empty.
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, errors.New("invalid configuration YAML or unknown field")
	}
	var trailing any
	// Even an empty trailing document is rejected; only the first document would otherwise apply.
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Config{}, errors.New("configuration must contain exactly one YAML document")
	}
	// Keep track of keys written by the operator before applying driver/node defaults.
	// A default host must not be mistaken for a conflicting explicit primary address.
	if err := cfg.Database.recordYAMLFields(data); err != nil {
		return Config{}, errors.New("invalid configuration YAML or unknown field")
	}
	cfg.Database = cfg.Database.Normalize()
	if err := Validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{2,63}$`)

// Validate names fields without including their values, because operator mistakes can
// put a password into any scalar, including a malformed port or timeout.
func Validate(c Config) error {
	invalid := func(field string) error { return fmt.Errorf("invalid configuration field: %s", field) }
	// Reject unknown deployment modes rather than guessing which security defaults were intended.
	if strings.TrimSpace(c.App.Name) == "" || len(c.App.Name) > 100 {
		return invalid("app.name")
	}
	if !oneOf(c.App.Environment, "development", "test", "production") {
		return invalid("app.environment")
	}
	_, port, err := net.SplitHostPort(c.HTTP.Addr)
	if err != nil {
		return invalid("http.addr")
	}
	p, err := strconv.Atoi(port)
	// Port zero asks the OS for a random port, which is unsuitable for a configured service.
	if err != nil || p < 1 || p > 65535 {
		return invalid("http.addr")
	}
	for name, d := range map[string]time.Duration{
		"http.read_header_timeout": c.HTTP.ReadHeaderTimeout, "http.read_timeout": c.HTTP.ReadTimeout,
		"http.write_timeout": c.HTTP.WriteTimeout, "http.idle_timeout": c.HTTP.IdleTimeout,
		"http.shutdown_timeout": c.HTTP.ShutdownTimeout, "http.request_timeout": c.HTTP.RequestTimeout,
	} {
		// Nonzero finite budgets are required even in development to expose slow-path problems early.
		if d < time.Millisecond || d > 10*time.Minute {
			return invalid(name)
		}
	}
	if c.HTTP.ReadHeaderTimeout > c.HTTP.ReadTimeout {
		return invalid("http.read_header_timeout")
	}
	if c.HTTP.RequestTimeout >= c.HTTP.WriteTimeout {
		return invalid("http.request_timeout")
	}
	if err := validateDatabase(c.Database); err != nil {
		return err
	}
	// Session TTL cannot truncate fractional seconds or grow into an effectively permanent token.
	if c.Auth.SessionTTL < time.Minute || c.Auth.SessionTTL > 30*24*time.Hour || c.Auth.SessionTTL%time.Second != 0 {
		return invalid("auth.session_ttl")
	}
	// Session count and login-attempt count protect different resources: retained tokens and CPU.
	if c.Auth.MaxSessions < 1 || c.Auth.MaxSessions > 100 {
		return invalid("auth.max_sessions")
	}
	if c.Auth.LoginAttemptsPerMinute < 1 || c.Auth.LoginAttemptsPerMinute > 10000 {
		return invalid("auth.login_attempts_per_minute")
	}
	// Initial and subsequent login validation share the same username grammar.
	if !usernamePattern.MatchString(c.Bootstrap.AdminUsername) {
		return invalid("bootstrap.admin_username")
	}
	// bcrypt accepts at most 72 bytes; checking bytes here also handles multibyte passwords safely.
	if !validSecret(c.Bootstrap.AdminPassword, 12, 72) {
		return invalid("bootstrap.admin_password")
	}
	key, err := base64.StdEncoding.DecodeString(c.Security.EncryptionKey)
	// AES-256 requires exactly 32 bytes, not a 32-character passphrase.
	if err != nil || len(key) != 32 || !validSecret(c.Security.EncryptionKey, 40, 64) {
		return invalid("security.encryption_key")
	}
	// Configuration lists are bounded too; bootstrap is not a reason to allow arbitrary allocation.
	if len(c.Security.TrustedProxies) > 100 {
		return invalid("security.trusted_proxies")
	}
	for _, proxy := range c.Security.TrustedProxies {
		// CIDRs support container/reverse-proxy networks; hostnames are not a stable trust boundary.
		if net.ParseIP(proxy) == nil {
			if _, _, err := net.ParseCIDR(proxy); err != nil {
				return invalid("security.trusted_proxies")
			}
		}
	}
	if len(c.Security.CORSOrigins) > 100 {
		return invalid("security.cors_origins")
	}
	for _, origin := range c.Security.CORSOrigins {
		// An origin contains only scheme, host, and optional port; paths are never sent in Origin.
		u, err := url.Parse(origin)
		if err != nil || !oneOf(u.Scheme, "http", "https") || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(u.Host, "* \t\r\n") {
			return invalid("security.cors_origins")
		}
	}
	// Set the body ceiling before binding JSON; validators only run after decoding has allocated memory.
	if c.Security.MaxBodyBytes < 1024 || c.Security.MaxBodyBytes > 32<<20 {
		return invalid("security.max_body_bytes")
	}
	if !oneOf(c.Logging.Level, "debug", "info", "warn", "error") {
		return invalid("logging.level")
	}
	if !oneOf(c.Logging.Format, "json", "text") {
		return invalid("logging.format")
	}
	return nil
}

func oneOf(value string, allowed ...string) bool {
	for _, item := range allowed {
		if item == value {
			return true
		}
	}
	return false
}

// Example placeholders must never silently become deployable credentials.
func validSecret(value string, min, max int) bool {
	// Surrounding whitespace is usually an accidental YAML edit and should fail loudly.
	// Embedded special characters remain valid because connection URLs escape them safely.
	if len(value) < min || len(value) > max || strings.TrimSpace(value) != value || strings.ContainsRune(value, 0) {
		return false
	}
	lower := strings.ToLower(value)
	for _, marker := range []string{"change-me", "changeme", "change_me", "replace-me", "replace_me", "replace-with", "replace_with", "your-password", "your_password", "example-password", "placeholder"} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	return true
}

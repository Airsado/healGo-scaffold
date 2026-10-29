package configs

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Database describes one driver and one logical database. The flat connection
// fields remain compatible with the original single-PostgreSQL configuration.
type Database struct {
	// Driver belongs to the logical database, rather than to each node. A replica
	// therefore cannot silently use a different SQL dialect from its primary.
	Driver string `yaml:"driver"`
	Host   string `yaml:"host"`
	Port   int    `yaml:"port"`
	Name   string `yaml:"name"`
	User   string `yaml:"user"`
	// Credentials are bootstrap secrets, never editable database-backed settings.
	Password string `yaml:"password" json:"-"`
	SSLMode  string `yaml:"ssl_mode"`
	// TLS accepts true or false for MySQL. Certificate verification cannot be disabled selectively.
	TLS string `yaml:"tls"`
	// Connection pools are bounded independently for the primary and each replica.
	MaxOpen int `yaml:"max_open"`
	// Zero idle connections is valid, unlike zero open connections which means unlimited.
	MaxIdle int `yaml:"max_idle"`
	// Lifetime rotates old connections; idle time releases unused pooled capacity.
	ConnMaxLifetime time.Duration `yaml:"conn_max_lifetime"`
	ConnMaxIdleTime time.Duration `yaml:"conn_max_idle_time"`
	// ConnectTimeout bounds an individual dial. Retries never replace the request context.
	ConnectTimeout time.Duration `yaml:"connect_timeout"`
	// Retry policy applies to opening each configured node during startup.
	RetryDelay    time.Duration `yaml:"retry_delay"`
	RetryAttempts int           `yaml:"retry_attempts"`
	AutoMigrate   bool          `yaml:"auto_migrate"`
	// Primary replaces the flat connection fields, while pool fields remain shared defaults.
	Primary *Endpoint `yaml:"primary"`
	// The first release intentionally supports one replica, keeping failure behavior explicit.
	Replicas          []Endpoint `yaml:"replicas"`
	FallbackToPrimary bool       `yaml:"fallback_to_primary"`

	// Presence distinguishes an omitted value from an explicitly invalid zero or empty value.
	// It is collected separately so KnownFields continues to validate all nested YAML fields.
	provided   map[string]bool
	normalized bool
}

// Endpoint is one physical server in the same driver/database topology. Replicas
// must supply their own host/user/password; the primary password is never copied.
type Endpoint struct {
	// Host and credentials identify a physical node; the remaining connection values
	// may inherit from the primary. Explicit YAML zeros still override inherited values.
	Host            string        `yaml:"host"`
	Port            int           `yaml:"port"`
	Name            string        `yaml:"name"`
	User            string        `yaml:"user"`
	Password        string        `yaml:"password" json:"-"`
	SSLMode         string        `yaml:"ssl_mode"`
	TLS             string        `yaml:"tls"`
	MaxOpen         int           `yaml:"max_open"`
	MaxIdle         int           `yaml:"max_idle"`
	ConnMaxLifetime time.Duration `yaml:"conn_max_lifetime"`
	ConnMaxIdleTime time.Duration `yaml:"conn_max_idle_time"`
	ConnectTimeout  time.Duration `yaml:"connect_timeout"`
	provided        map[string]bool
}

func (Endpoint) String() string     { return "Endpoint{connection values redacted}" }
func (e Endpoint) GoString() string { return e.String() }

// EffectiveDriver preserves programmatically constructed pre-MySQL configurations.
// It does not accept misspellings; Validate rejects every nonempty unknown driver.
func (d Database) EffectiveDriver() string {
	if d.Driver == "" {
		return "postgres"
	}
	return d.Driver
}

// Normalize applies driver defaults without replacing explicit invalid YAML.
// It preserves topology, and is idempotent so startup providers may safely reuse it.
func (d Database) Normalize() Database {
	d.Driver = d.EffectiveDriver()
	// Loaded values include legacy defaults. Presence tells us whether 5432 came
	// from the operator or from defaults(), so choosing MySQL can safely select 3306.
	if d.Port == 0 && !d.provided["port"] || d.provided != nil && !d.provided["port"] && !d.normalized {
		d.Port = 5432
		if d.Driver == "mysql" {
			d.Port = 3306
		}
	}
	if d.Driver == "mysql" {
		// The legacy PostgreSQL default does not count as an explicit MySQL setting.
		if d.provided != nil && !d.provided["ssl_mode"] && !d.normalized {
			d.SSLMode = ""
		}
		if d.TLS == "" && !d.provided["tls"] {
			// An explicit empty tls is invalid; only an omitted value gets the secure default.
			d.TLS = "true"
		}
	} else if d.SSLMode == "" && !d.provided["ssl_mode"] {
		d.SSLMode = "require"
	}
	if d.Primary != nil {
		// Resolve primary overrides after choosing driver defaults; doing this before
		// the port default would replace a custom primary port on a second Normalize call.
		d = overlayEndpoint(d, *d.Primary)
	}
	// Remember that the flat fields already contain effective values. This also
	// keeps callers using PrimaryConfig from reapplying the legacy load defaults.
	d.normalized = true
	return d
}

// PrimaryConfig returns a conventional flat config for the writer connection.
// Returning values keeps callers from modifying the original topology by accident.
func (d Database) PrimaryConfig() Database {
	d = d.Normalize()
	// A connector should open precisely one node. Carrying topology into it would
	// make resource ownership and recursive replica construction ambiguous.
	d.Primary = nil
	d.Replicas = nil
	return d
}

// ReplicaConfigs inherits pool/TLS settings and the database name from primary.
// Clearing host and credentials first prevents accidental privileged credential reuse.
func (d Database) ReplicaConfigs() []Database {
	primary := d.PrimaryConfig()
	result := make([]Database, 0, len(d.Replicas))
	for _, endpoint := range d.Replicas {
		replica := primary
		// In particular, a missing password must fail validation instead of causing
		// the read pool to authenticate with the privileged primary account.
		replica.Host, replica.User, replica.Password = "", "", ""
		replica = overlayEndpoint(replica, endpoint)
		result = append(result, replica)
	}
	return result
}

func overlayEndpoint(d Database, e Endpoint) Database {
	// Loaded endpoints use presence metadata. Programmatic endpoints treat nonzero
	// values as overrides; MaxIdle=0 should be supplied with YAML when inherited.
	setString := func(key string, value string, target *string) {
		// The presence check intentionally allows an explicit empty string through;
		// validation can then name the invalid field instead of hiding the mistake.
		if e.provided[key] || value != "" {
			*target = value
		}
	}
	setInt := func(key string, value int, target *int) {
		// max_idle: 0 is a real instruction to keep no idle connections.
		if e.provided[key] || value != 0 {
			*target = value
		}
	}
	setDuration := func(key string, value time.Duration, target *time.Duration) {
		// Explicit zero timeouts are also retained so they cannot disable budgets silently.
		if e.provided[key] || value != 0 {
			*target = value
		}
	}
	setString("host", e.Host, &d.Host)
	setString("name", e.Name, &d.Name)
	setString("user", e.User, &d.User)
	setString("password", e.Password, &d.Password)
	setString("ssl_mode", e.SSLMode, &d.SSLMode)
	setString("tls", e.TLS, &d.TLS)
	setInt("port", e.Port, &d.Port)
	setInt("max_open", e.MaxOpen, &d.MaxOpen)
	setInt("max_idle", e.MaxIdle, &d.MaxIdle)
	setDuration("conn_max_lifetime", e.ConnMaxLifetime, &d.ConnMaxLifetime)
	setDuration("conn_max_idle_time", e.ConnMaxIdleTime, &d.ConnMaxIdleTime)
	setDuration("connect_timeout", e.ConnectTimeout, &d.ConnectTimeout)
	return d
}

func validateDatabase(raw Database) error {
	// Validation errors identify a fixed field path and never include its scalar.
	// A password pasted into the wrong field must stay out of startup logs too.
	invalid := func(field string) error { return fmt.Errorf("invalid configuration field: %s", field) }
	if !oneOf(raw.EffectiveDriver(), "postgres", "mysql") {
		return invalid("database.driver")
	}
	if raw.provided["primary"] && raw.Primary == nil {
		// A null primary is not a request to fall back to unrelated flat credentials.
		return invalid("database.primary")
	}
	if raw.Primary != nil {
		// Pool limits are shared defaults, but two different addresses/credentials
		// would make it unclear which physical server receives migrations and writes.
		for _, key := range []string{"host", "port", "name", "user", "password", "ssl_mode", "tls"} {
			if raw.provided[key] {
				return invalid("database.primary")
			}
		}
		if raw.provided == nil && !raw.normalized && (raw.Host != "" || raw.Port != 0 || raw.Name != "" || raw.User != "" || raw.Password != "" || raw.SSLMode != "" || raw.TLS != "") {
			// Go struct literals have no YAML presence metadata. Nonzero connection
			// fields are treated as explicit until the caller normalizes the structure.
			return invalid("database.primary")
		}
	}
	if len(raw.Replicas) > 1 {
		// Keep the first release's connection capacity and retry semantics predictable.
		return invalid("database.replicas")
	}
	d := raw.Normalize()
	primary := d.PrimaryConfig()
	// The path names the effective primary declaration rather than a default field
	// that the operator never wrote, making nested configuration errors actionable.
	prefix := "database"
	if d.Primary != nil {
		prefix += ".primary"
	}
	if err := validateEndpoint(primary, prefix); err != nil {
		return err
	}
	for i, replica := range d.ReplicaConfigs() {
		prefix := fmt.Sprintf("database.replicas[%d]", i)
		if replica.Name != primary.Name {
			// Read routing is replication, not sharding: both pools address one database.
			return invalid(prefix + ".name")
		}
		if err := validateEndpoint(replica, prefix); err != nil {
			return err
		}
	}
	if d.RetryDelay < time.Millisecond || d.RetryDelay > 30*time.Second {
		// Per-node retries remain bounded even when a whole topology is unavailable.
		return invalid("database.retry_delay")
	}
	if d.RetryAttempts < 1 || d.RetryAttempts > 10 {
		return invalid("database.retry_attempts")
	}
	return nil
}

func validateEndpoint(d Database, prefix string) error {
	invalid := func(field string) error { return fmt.Errorf("invalid configuration field: %s.%s", prefix, field) }
	// The connector builds its own DSN; URL fragments cannot override another field.
	if d.Host == "" || strings.ContainsAny(d.Host, " \t\r\n\x00/?#@") {
		return invalid("host")
	}
	if d.Port < 1 || d.Port > 65535 {
		return invalid("port")
	}
	nameLimit, userLimit := 63, 63
	// Identifier limits differ between the supported engines; reject truncation
	// instead of opening a similarly named database or authenticating as another user.
	if d.Driver == "mysql" {
		nameLimit, userLimit = 64, 32
	}
	if strings.TrimSpace(d.Name) == "" || len(d.Name) > nameLimit || strings.ContainsRune(d.Name, 0) {
		return invalid("name")
	}
	if strings.TrimSpace(d.User) == "" || len(d.User) > userLimit || strings.ContainsRune(d.User, 0) {
		return invalid("user")
	}
	if !validSecret(d.Password, 12, 1024) {
		// Placeholder and surrounding-whitespace checks match single-node startup.
		return invalid("password")
	}
	if d.Driver == "mysql" {
		// Driver-specific transport options cannot be mixed. A PostgreSQL ssl_mode
		// must not give an operator a false impression of MySQL transport security.
		if !oneOf(d.TLS, "true", "false") {
			return invalid("tls")
		}
		if d.SSLMode != "" {
			return invalid("ssl_mode")
		}
	} else {
		if !oneOf(d.SSLMode, "disable", "allow", "prefer", "require", "verify-ca", "verify-full") {
			return invalid("ssl_mode")
		}
		if d.TLS != "" {
			return invalid("tls")
		}
	}
	if d.MaxOpen < 1 || d.MaxOpen > 1000 {
		// database/sql interprets zero as unlimited, which is unsuitable for a template.
		return invalid("max_open")
	}
	if d.MaxIdle < 0 || d.MaxIdle > d.MaxOpen {
		return invalid("max_idle")
	}
	if d.ConnMaxLifetime < time.Second || d.ConnMaxLifetime > 24*time.Hour {
		return invalid("conn_max_lifetime")
	}
	if d.ConnMaxIdleTime < time.Second || d.ConnMaxIdleTime > d.ConnMaxLifetime {
		// Idle connections should retire no later than the maximum connection age.
		return invalid("conn_max_idle_time")
	}
	if d.ConnectTimeout < time.Millisecond || d.ConnectTimeout > time.Minute {
		return invalid("connect_timeout")
	}
	return nil
}

func (d *Database) recordYAMLFields(data []byte) error {
	// Decode metadata only after the ordinary KnownFields decoder succeeded.
	// Using custom UnmarshalYAML on Database would otherwise bypass strict nested fields.
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return err
	}
	root := mappingValues(&document)
	database := mappingValues(root["database"])
	// Save only field names, not scalar text; validation never needs a secret copy.
	d.provided = fieldSet(database)
	if d.Primary != nil {
		// Each endpoint needs its own set to distinguish omission from max_idle: 0.
		d.Primary.provided = fieldSet(mappingValues(database["primary"]))
	}
	if node := database["replicas"]; node != nil {
		node = resolveNode(node)
		// Strict decoding already verified the sequence type and endpoint count.
		for i, endpoint := range node.Content {
			if i < len(d.Replicas) {
				d.Replicas[i].provided = fieldSet(mappingValues(endpoint))
			}
		}
	}
	return nil
}

func fieldSet(values map[string]*yaml.Node) map[string]bool {
	// A nonnil empty map means a file was decoded but the database mapping was absent.
	fields := make(map[string]bool, len(values))
	for key := range values {
		fields[key] = true
	}
	return fields
}

func resolveNode(node *yaml.Node) *yaml.Node {
	// yaml.Node represents the document wrapper and aliases separately from values.
	// The strict decode has already rejected self-referential aliases in typed fields.
	for node != nil && (node.Kind == yaml.DocumentNode || node.Kind == yaml.AliasNode) {
		if node.Kind == yaml.AliasNode {
			node = node.Alias
		} else if len(node.Content) > 0 {
			node = node.Content[0]
		} else {
			return nil
		}
	}
	return node
}

func mappingValues(node *yaml.Node) map[string]*yaml.Node {
	values := make(map[string]*yaml.Node)
	node = resolveNode(node)
	if node == nil || node.Kind != yaml.MappingNode {
		return values
	}
	// YAML merge keys have the same meaning as explicit fields for conflict checks.
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if key.Value == "<<" {
			merged := resolveNode(value)
			if merged != nil && merged.Kind == yaml.SequenceNode {
				for _, item := range merged.Content {
					for key, value := range mappingValues(item) {
						if values[key] == nil {
							values[key] = value
						}
					}
				}
			} else {
				for key, value := range mappingValues(value) {
					values[key] = value
				}
			}
		}
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value != "<<" {
			// Explicit mapping entries override merges, exactly as the YAML decoder does.
			values[node.Content[i].Value] = node.Content[i+1]
		}
	}
	return values
}

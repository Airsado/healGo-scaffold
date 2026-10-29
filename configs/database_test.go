package configs

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func databaseYAML(database string) string {
	return "database:\n" + database + "bootstrap:\n  admin_password: private-admin-credential\nsecurity:\n  encryption_key: " + validConfig().Security.EncryptionKey + "\n"
}

func TestDatabaseDriverDefaultsAndLegacyCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, database, driver, tls, ssl string
		port                             int
	}{
		{"legacy postgres", "  password: private-primary-credential\n", "postgres", "", "require", 5432},
		{"mysql secure default", "  driver: mysql\n  password: private-primary-credential\n", "mysql", "true", "", 3306},
		{"mysql local network", "  driver: mysql\n  password: private-primary-credential\n  tls: false\n", "mysql", "false", "", 3306},
		{"custom mysql port", "  driver: mysql\n  port: 13306\n  password: private-primary-credential\n  tls: true\n", "mysql", "true", "", 13306},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := loadYAML(t, databaseYAML(tc.database))
			if err != nil {
				t.Fatal(err)
			}
			d := c.Database.PrimaryConfig()
			if d.EffectiveDriver() != tc.driver || d.Port != tc.port || d.TLS != tc.tls || d.SSLMode != tc.ssl || !c.Database.FallbackToPrimary {
				t.Fatal("driver, connection or fallback defaults differ from the documented defaults")
			}
			if err := Validate(c); err != nil {
				t.Fatalf("normalized config is not valid: %v", err)
			}
		})
	}
}

const topologyYAML = `  driver: mysql
  max_open: 12
  max_idle: 4
  fallback_to_primary: false
  primary:
    host: mysql-primary
    port: 13306
    name: heago
    user: heago_writer
    password: private-primary-credential
    tls: false
  replicas:
    - host: mysql-replica
      user: heago_reader
      password: private-replica-credential
      max_open: 6
      max_idle: 0
`

func TestTopologyInheritanceAndIndependentCredentials(t *testing.T) {
	c, err := loadYAML(t, databaseYAML(topologyYAML))
	if err != nil {
		t.Fatal(err)
	}
	primary := c.Database.PrimaryConfig()
	replicas := c.Database.ReplicaConfigs()
	if len(replicas) != 1 || primary.Primary != nil || len(primary.Replicas) != 0 {
		t.Fatal("connection configs must be flat and keep the configured node count")
	}
	r := replicas[0]
	if primary.MaxOpen != 12 || primary.MaxIdle != 4 || r.MaxOpen != 6 || r.MaxIdle != 0 || c.Database.FallbackToPrimary {
		t.Fatal("explicit pool/fallback values were lost during normalization")
	}
	if r.Name != primary.Name || r.Port != primary.Port || r.TLS != primary.TLS || r.ConnMaxLifetime != primary.ConnMaxLifetime {
		t.Fatal("replica did not inherit driver/database/pool defaults from primary")
	}
	if r.Host != "mysql-replica" || r.User != "heago_reader" || r.Password != "private-replica-credential" || r.Password == primary.Password {
		t.Fatal("replica credentials were replaced or inherited from primary")
	}
	// Normalizing repeatedly must not reset the primary's custom port or replica limits.
	second := c.Database.Normalize().Normalize().ReplicaConfigs()[0]
	if second.Port != r.Port || second.MaxIdle != 0 || second.Password != r.Password {
		t.Fatal("normalization is not idempotent")
	}
}

func TestInvalidTopologyAndDriverOptions(t *testing.T) {
	cases := map[string]string{
		"unknown driver":         "  driver: sqlite\n  password: private-primary-credential\n",
		"null primary":           "  primary: null\n  password: private-primary-credential\n",
		"mysql verify skipped":   strings.Replace(topologyYAML, "tls: false", "tls: skip-verify", 1),
		"mysql postgres SSL":     strings.Replace(topologyYAML, "tls: false", "ssl_mode: disable", 1),
		"different database":     strings.Replace(topologyYAML, "- host: mysql-replica", "- host: mysql-replica\n      name: other_database", 1),
		"missing replica host":   strings.Replace(topologyYAML, "- host: mysql-replica", "- host: ''", 1),
		"missing replica user":   strings.Replace(topologyYAML, "      user: heago_reader\n", "", 1),
		"missing replica secret": strings.Replace(topologyYAML, "      password: private-replica-credential\n", "", 1),
		"unknown nested field":   strings.Replace(topologyYAML, "      max_open: 6", "      retry_attampts: 6", 1),
		"mixed endpoint driver":  strings.Replace(topologyYAML, "- host: mysql-replica", "- host: mysql-replica\n      driver: postgres", 1),
		"zero replica pool":      strings.Replace(topologyYAML, "max_open: 6", "max_open: 0", 1),
		"zero primary port":      strings.Replace(topologyYAML, "port: 13306", "port: 0", 1),
		"zero primary timeout":   strings.Replace(topologyYAML, "    port: 13306", "    port: 13306\n    connect_timeout: 0s", 1),
		"too many replicas":      topologyYAML + "    - host: another-replica\n      user: reader\n      password: private-other-credential\n",
		"flat alias conflict":    strings.Replace(topologyYAML, "  primary:", "  host: &address mysql-primary\n  primary:", 1),
	}
	// Even a flat value equal to its default is an ambiguous second connection declaration.
	for key, value := range map[string]string{"host": "127.0.0.1", "port": "5432", "name": "heago", "user": "heago", "password": "private-primary-credential", "ssl_mode": "require", "tls": "false"} {
		cases["mixed flat "+key] = strings.Replace(topologyYAML, "  primary:", "  "+key+": "+value+"\n  primary:", 1)
	}
	for name, database := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadYAML(t, databaseYAML(database))
			if err == nil {
				t.Fatal("invalid database topology accepted")
			}
			if strings.Contains(err.Error(), "private-") || strings.Contains(err.Error(), "mysql-replica") {
				t.Fatal("configuration error leaked a connection value")
			}
		})
	}
}

func TestDatabaseIdentifierAndTransportBoundaries(t *testing.T) {
	for _, tc := range []struct {
		driver, key, value string
		valid              bool
	}{
		{"mysql", "name", strings.Repeat("a", 64), true},
		{"mysql", "name", strings.Repeat("a", 65), false},
		{"mysql", "user", strings.Repeat("a", 32), true},
		{"mysql", "user", strings.Repeat("a", 33), false},
		{"postgres", "name", strings.Repeat("a", 63), true},
		{"postgres", "name", strings.Repeat("a", 64), false},
		{"postgres", "tls", "true", false},
		{"mysql", "tls", "''", false},
		{"postgres", "ssl_mode", "''", false},
		{"mysql", "port", "0", false},
	} {
		t.Run(tc.driver+" "+tc.key+" "+fmt.Sprint(len(tc.value)), func(t *testing.T) {
			_, err := loadYAML(t, databaseYAML("  driver: "+tc.driver+"\n  password: private-primary-credential\n  "+tc.key+": "+tc.value+"\n"))
			if (err == nil) != tc.valid {
				t.Fatalf("unexpected validation outcome: %v", err)
			}
		})
	}
}

func TestProgrammaticDatabaseAndEndpointRedaction(t *testing.T) {
	d := Database{Driver: "mysql", MaxOpen: 12, MaxIdle: 4, ConnMaxLifetime: time.Hour, ConnMaxIdleTime: time.Minute,
		ConnectTimeout: time.Second, RetryDelay: time.Second, RetryAttempts: 2,
		Primary:  &Endpoint{Host: "writer", Name: "app", User: "writer", Password: "private-primary-credential"},
		Replicas: []Endpoint{{Host: "reader", User: "reader", Password: "private-replica-credential"}}}
	if err := validateDatabase(d); err != nil {
		t.Fatal(err)
	}
	if d.PrimaryConfig().Port != 3306 || d.ReplicaConfigs()[0].Password == d.Primary.Password {
		t.Fatal("programmatic endpoint normalization failed")
	}
	d.Replicas[0].Password = ""
	if err := validateDatabase(d); err == nil {
		t.Fatal("missing replica password inherited the privileged primary password")
	}
	for _, formatted := range []string{fmt.Sprint(d.Primary), fmt.Sprintf("%+v", d.Primary), fmt.Sprintf("%#v", d.Primary), fmt.Sprintf("%#v", d.Replicas)} {
		if strings.Contains(formatted, "credential") || strings.Contains(formatted, "writer") {
			t.Fatal("endpoint formatting leaked a connection value")
		}
	}
}

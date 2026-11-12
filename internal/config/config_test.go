package config

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestQueryAccountSetTogether(t *testing.T) {
	t.Setenv("AIO11Y_BACKEND_QUERY_USERNAME", "reader")
	t.Setenv("AIO11Y_BACKEND_QUERY_PASSWORD", "")
	if _, err := Load(); err == nil {
		t.Error("a query username without a password loaded")
	}
	t.Setenv("AIO11Y_BACKEND_QUERY_PASSWORD", "pw")
	if cfg, err := Load(); err != nil || cfg.Backend.QueryUsername != "reader" {
		t.Errorf("Load = %v", err)
	}
}

func TestQueryBasicAndBearerRefused(t *testing.T) {
	t.Setenv("AIO11Y_BACKEND_QUERY_USERNAME", "reader")
	t.Setenv("AIO11Y_BACKEND_QUERY_PASSWORD", "pw")
	t.Setenv("AIO11Y_BACKEND_QUERY_BEARER_TOKEN", "read-token")
	if _, err := Load(); err == nil {
		t.Error("a basic auth account and a bearer token loaded together")
	}
	t.Setenv("AIO11Y_BACKEND_QUERY_USERNAME", "")
	t.Setenv("AIO11Y_BACKEND_QUERY_PASSWORD", "")
	cfg, err := Load()
	if err != nil || cfg.Backend.QueryBearerToken != "read-token" {
		t.Errorf("Load = %+v, %v, want the bearer token alone", cfg, err)
	}
	if cfg != nil && cfg.Backend.QueryTLSInsecureSkipVerify {
		t.Error("QueryTLSInsecureSkipVerify is on by default")
	}
}

func TestBackendTimeouts(t *testing.T) {
	t.Setenv("AIO11Y_BACKEND_TIMEOUT", "")
	t.Setenv("AIO11Y_BACKEND_CONNECT_TIMEOUT", "3s")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Backend.Timeout != 0 || cfg.Backend.ConnectTimeout != 3*time.Second {
		t.Errorf("timeouts = %s, %s, want empty for the default and 3s", cfg.Backend.Timeout, cfg.Backend.ConnectTimeout)
	}
	t.Setenv("AIO11Y_BACKEND_MAX_IDS", "")
	t.Setenv("AIO11Y_BACKEND_MAX_CONCURRENT", "8")
	if cfg, err = Load(); err != nil || cfg.Backend.MaxIDs != 0 || cfg.Backend.MaxConcurrent != 8 {
		t.Errorf("counts = %+v, %v, want empty for the default and 8", cfg, err)
	}
	for _, bad := range []string{"0", "-1", "many"} {
		t.Setenv("AIO11Y_BACKEND_MAX_IDS", bad)
		if _, err := Load(); err == nil {
			t.Errorf("AIO11Y_BACKEND_MAX_IDS=%q loaded", bad)
		}
	}
	t.Setenv("AIO11Y_BACKEND_MAX_IDS", "")
	for _, bad := range []string{"0", "0s", "-1s", "soon"} {
		t.Setenv("AIO11Y_BACKEND_CONNECT_TIMEOUT", bad)
		if _, err := Load(); err == nil {
			t.Errorf("AIO11Y_BACKEND_CONNECT_TIMEOUT=%q loaded", bad)
		}
	}
}

func TestBackendEnvDoesNotShadowBackendKeys(t *testing.T) {
	t.Setenv("AIO11Y_BACKEND", "victoria")
	t.Setenv("AIO11Y_BACKEND_QUERY_URL", "http://vm-a:8427/,http://vm-b:8427")
	t.Setenv("AIO11Y_BACKEND_WRITE_URL", "http://vm-a:8427/opentelemetry")
	t.Setenv("AIO11Y_BACKEND_WRITE_USERNAME", "writer")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	b := cfg.Backend
	if b.Provider != BackendVictoria {
		t.Errorf("Provider = %q, want %q", b.Provider, BackendVictoria)
	}
	if want := []string{"http://vm-a:8427", "http://vm-b:8427"}; !slices.Equal(b.QueryURLs, want) {
		t.Errorf("QueryURLs = %q, want %q", b.QueryURLs, want)
	}
	if want := []string{"http://vm-a:8427/opentelemetry"}; !slices.Equal(b.WriteURLs, want) {
		t.Errorf("WriteURLs = %q, want %q", b.WriteURLs, want)
	}
	if b.WriteUsername != "writer" {
		t.Errorf("WriteUsername = %q, want writer", b.WriteUsername)
	}
}

// TestTemplateVariablesLoad gives every variable conf/template-setup.env
// exports a value of its own and checks each lands in Config. A variable that
// cannot be set together with others is loaded apart, with those cleared.
func TestTemplateVariablesLoad(t *testing.T) {
	cases := map[string]struct {
		value string
		got   func(*Config) any
		want  any
		apart []string
	}{
		"AIO11Y_SERVER_PORT":                            {"18094", func(c *Config) any { return c.Server.Port }, 18094, nil},
		"AIO11Y_AUTH_TOKEN":                             {"tok", func(c *Config) any { return c.Auth.Token }, "tok", nil},
		"AIO11Y_TUMBLEBUG_BASE_URL":                     {"http://tb:1323/tumblebug/", func(c *Config) any { return c.Tumblebug.BaseURL }, "http://tb:1323/tumblebug", nil},
		"AIO11Y_TUMBLEBUG_USERNAME":                     {"tbuser", func(c *Config) any { return c.Tumblebug.Username }, "tbuser", nil},
		"AIO11Y_TUMBLEBUG_PASSWORD":                     {"tbpw", func(c *Config) any { return c.Tumblebug.Password }, "tbpw", nil},
		"AIO11Y_TUMBLEBUG_CREDENTIAL_HOLDER":            {"holder", func(c *Config) any { return c.Tumblebug.CredentialHolder }, "holder", nil},
		"AIO11Y_TUMBLEBUG_TIMEOUT":                      {"7m", func(c *Config) any { return c.Tumblebug.Timeout }, 7 * time.Minute, nil},
		"AIO11Y_DB_HOST":                                {"dbhost", func(c *Config) any { return c.DB.Host }, "dbhost", nil},
		"AIO11Y_DB_PORT":                                {"3307", func(c *Config) any { return c.DB.Port }, 3307, nil},
		"AIO11Y_DB_NAME":                                {"dbname", func(c *Config) any { return c.DB.Name }, "dbname", nil},
		"AIO11Y_DB_USER":                                {"dbuser", func(c *Config) any { return c.DB.User }, "dbuser", nil},
		"AIO11Y_DB_PASSWORD":                            {"dbpw", func(c *Config) any { return c.DB.Password }, "dbpw", nil},
		"AIO11Y_DB_CONNECT_TIMEOUT":                     {"6s", func(c *Config) any { return c.DB.ConnectTimeout }, 6 * time.Second, nil},
		"AIO11Y_DB_READ_TIMEOUT":                        {"31s", func(c *Config) any { return c.DB.ReadTimeout }, 31 * time.Second, nil},
		"AIO11Y_DB_WRITE_TIMEOUT":                       {"32s", func(c *Config) any { return c.DB.WriteTimeout }, 32 * time.Second, nil},
		"AIO11Y_BACKEND":                                {"VICTORIA", func(c *Config) any { return c.Backend.Provider }, BackendVictoria, nil},
		"AIO11Y_BACKEND_QUERY_URL":                      {"http://q1, http://q2/", func(c *Config) any { return strings.Join(c.Backend.QueryURLs, ",") }, "http://q1,http://q2", nil},
		"AIO11Y_BACKEND_WRITE_URL":                      {"http://w1/", func(c *Config) any { return strings.Join(c.Backend.WriteURLs, ",") }, "http://w1", nil},
		"AIO11Y_BACKEND_LOGS_WRITE_URL":                 {"http://l1/, http://l2", func(c *Config) any { return strings.Join(c.Backend.LogsWriteURLs, ",") }, "http://l1,http://l2", nil},
		"AIO11Y_BACKEND_TRACES_WRITE_URL":               {"http://t1/", func(c *Config) any { return strings.Join(c.Backend.TracesWriteURLs, ",") }, "http://t1", nil},
		"AIO11Y_BACKEND_TIMEOUT":                        {"45s", func(c *Config) any { return c.Backend.Timeout }, 45 * time.Second, nil},
		"AIO11Y_BACKEND_CONNECT_TIMEOUT":                {"4s", func(c *Config) any { return c.Backend.ConnectTimeout }, 4 * time.Second, nil},
		"AIO11Y_BACKEND_MAX_CONCURRENT":                 {"3", func(c *Config) any { return c.Backend.MaxConcurrent }, 3, nil},
		"AIO11Y_BACKEND_MAX_IDS":                        {"100", func(c *Config) any { return c.Backend.MaxIDs }, 100, nil},
		"AIO11Y_BACKEND_QUERY_USERNAME":                 {"reader", func(c *Config) any { return c.Backend.QueryUsername }, "reader", nil},
		"AIO11Y_BACKEND_QUERY_PASSWORD":                 {"readpw", func(c *Config) any { return c.Backend.QueryPassword }, "readpw", nil},
		"AIO11Y_BACKEND_WRITE_USERNAME":                 {"writer", func(c *Config) any { return c.Backend.WriteUsername }, "writer", nil},
		"AIO11Y_BACKEND_PARTIAL_RESPONSE":               {"true", func(c *Config) any { return c.Backend.PartialResponse }, true, nil},
		"AIO11Y_BACKEND_QUERY_BEARER_TOKEN":             {"read-token", func(c *Config) any { return c.Backend.QueryBearerToken }, "read-token", []string{"AIO11Y_BACKEND_QUERY_USERNAME", "AIO11Y_BACKEND_QUERY_PASSWORD"}},
		"AIO11Y_BACKEND_QUERY_TLS_CA_FILE":              {" /etc/aio11y/ca.pem ", func(c *Config) any { return c.Backend.QueryTLSCAFile }, "/etc/aio11y/ca.pem", nil},
		"AIO11Y_BACKEND_QUERY_TLS_INSECURE_SKIP_VERIFY": {"true", func(c *Config) any { return c.Backend.QueryTLSInsecureSkipVerify }, true, nil},
		"AIO11Y_COLLECTOR_TELEMETRY_PORT":               {"8888", func(c *Config) any { return c.Collector.TelemetryPort }, 8888, nil},
		"AIO11Y_COLLECTOR_TLS_CA_FILE":                  {" /etc/aio11y-otelcol/ca.pem ", func(c *Config) any { return c.Collector.TLSCAFile }, "/etc/aio11y-otelcol/ca.pem", nil},
		"AIO11Y_COLLECTOR_TLS_INSECURE_SKIP_VERIFY":     {"true", func(c *Config) any { return c.Collector.TLSInsecureSkipVerify }, true, nil},
		"AIO11Y_COLLECTOR_STORAGE_DIR":                  {"/srv/queue", func(c *Config) any { return c.Collector.StorageDir }, "/srv/queue", nil},
	}

	raw, err := os.ReadFile("../../conf/template-setup.env")
	if err != nil {
		t.Fatal(err)
	}
	exported := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		name, _, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(line), "export "), "=")
		if !ok || strings.HasPrefix(name, "#") {
			continue
		}
		exported[name] = true
	}
	for name := range exported {
		if _, ok := cases[name]; !ok {
			t.Errorf("template variable %s has no case here", name)
		}
	}
	for name := range cases {
		if !exported[name] {
			t.Errorf("case %s is not in the template", name)
		}
	}

	for name, c := range cases {
		if c.apart == nil {
			t.Setenv(name, c.value)
		}
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for name, c := range cases {
		if c.apart == nil {
			if got := c.got(cfg); got != c.want {
				t.Errorf("%s=%q: got %v, want %v", name, c.value, got, c.want)
			}
		}
	}
	for name, c := range cases {
		if c.apart == nil {
			continue
		}
		t.Run(name, func(t *testing.T) {
			for _, other := range c.apart {
				t.Setenv(other, "")
			}
			t.Setenv(name, c.value)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := c.got(cfg); got != c.want {
				t.Errorf("%s=%q: got %v, want %v", name, c.value, got, c.want)
			}
		})
	}
}

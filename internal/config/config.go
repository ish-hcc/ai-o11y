// Package config loads runtime configuration from environment variables.
package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
)

const envPrefix = "AIO11Y"

// Config holds all runtime configuration for the service.
type Config struct {
	Server    ServerConfig
	Auth      AuthConfig
	Tumblebug TumblebugConfig
	DB        DBConfig
	Backend   BackendConfig
	Collector CollectorConfig
}

// CollectorConfig is about the collector configurations ai-o11y renders.
type CollectorConfig struct {
	// TelemetryPort is the localhost port collectors serve their own metrics
	// on; 0 turns them off.
	TelemetryPort int
	// StorageDir is the directory on a collector's host its disk queue is
	// kept in.
	StorageDir string
}

// BackendConfig is the metrics backend. The settings are the same whichever
// provider is selected; a zero value takes the provider's default. An empty
// query URL leaves queries and collector configuration unavailable.
type BackendConfig struct {
	// Provider is "thanos" or "victoria" (AIO11Y_BACKEND).
	Provider string
	// QueryURLs are addresses serving the Prometheus query API, in order of
	// preference, given as a comma-separated list; a query moves to the next
	// when one cannot be reached or answers with a 5xx.
	QueryURLs []string
	// WriteURLs are where collectors send samples over OTLP/HTTP, in order of
	// preference, given as a comma-separated list; a collector moves to the
	// next while one fails. The collector appends /v1/metrics to each.
	WriteURLs []string
	// Timeout bounds one query, across every query address it is tried at.
	// ConnectTimeout bounds connecting to one address. Left empty, each takes
	// the provider's default; a value given must be positive.
	Timeout        time.Duration
	ConnectTimeout time.Duration
	// MaxConcurrent is how many query chunks run at once; MaxIDs is how many
	// target ids one chunk selects. Left empty, each takes the provider's
	// default; a value given must be positive.
	MaxConcurrent int
	MaxIDs        int
	// PartialResponse lets a query answer from the stores that respond when
	// another fails, where the backend has several stores behind one query
	// address. Off by default: any failing store fails the query.
	PartialResponse bool
	// QueryUsername and QueryPassword are the basic auth account queries are
	// sent with. Both empty sends none; one without the other is an error.
	QueryUsername string
	QueryPassword string
	// WriteUsername is the basic auth account collectors write with. Its
	// password is not set here: each collector reads it from its own host's
	// environment.
	WriteUsername string
}

// Metrics backend providers AIO11Y_BACKEND selects.
const (
	BackendThanos   = "thanos"
	BackendVictoria = "victoria"
)

// DBConfig is the MariaDB that keeps the target table. An empty Host leaves
// the database out: the service starts, but nothing that needs the target
// table is available.
type DBConfig struct {
	Host           string
	Port           int
	Name           string
	User           string
	Password       string
	ConnectTimeout time.Duration
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration
}

// TumblebugConfig is where CB-Tumblebug is reached. An empty BaseURL leaves the
// tumblebug target source unregistered.
type TumblebugConfig struct {
	BaseURL          string
	Username         string
	Password         string
	CredentialHolder string
	Timeout          time.Duration
}

// ServerConfig is where the REST API listens.
type ServerConfig struct {
	Port int
}

// AuthConfig guards the REST API.
type AuthConfig struct {
	// Token is the bearer token every route but readiness and the API
	// documentation requires. Empty turns the check off.
	Token string
}

// Load reads configuration from environment variables and validates it.
// Every setting has a default, so the service starts with no configuration.
func Load() (*Config, error) {
	v := viper.New()
	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	v.SetDefault("server.port", 8094)
	v.SetDefault("tumblebug.timeout", 2*time.Minute)
	v.SetDefault("db.port", 3306)
	v.SetDefault("db.name", "aio11y")
	v.SetDefault("db.connect_timeout", 5*time.Second)
	v.SetDefault("db.read_timeout", 30*time.Second)
	v.SetDefault("db.write_timeout", 30*time.Second)
	v.SetDefault("backend.provider", BackendThanos)
	v.SetDefault("backend.partial_response", false)
	v.SetDefault("collector.storage_dir", "/var/lib/aio11y-otelcol/queue")

	// Every key is bound on its own rather than through AutomaticEnv: with it,
	// AIO11Y_BACKEND set would shadow every backend.* key as their parent.
	for _, key := range []string{
		"server.port",
		"auth.token",
		"tumblebug.base_url",
		"tumblebug.username",
		"tumblebug.password",
		"tumblebug.credential_holder",
		"tumblebug.timeout",
		"db.host",
		"db.port",
		"db.name",
		"db.user",
		"db.password",
		"db.connect_timeout",
		"db.read_timeout",
		"db.write_timeout",
		"backend.query_url",
		"backend.write_url",
		"backend.timeout",
		"backend.connect_timeout",
		"backend.max_concurrent",
		"backend.max_ids",
		"backend.partial_response",
		"backend.query_username",
		"backend.query_password",
		"backend.write_username",
		"collector.telemetry_port",
		"collector.storage_dir",
	} {
		if err := v.BindEnv(key); err != nil {
			return nil, fmt.Errorf("failed to bind env for %q: %w", key, err)
		}
	}

	// The provider is AIO11Y_BACKEND itself, not AIO11Y_BACKEND_PROVIDER.
	if err := v.BindEnv("backend.provider", envPrefix+"_BACKEND"); err != nil {
		return nil, fmt.Errorf("failed to bind env for %q: %w", "backend.provider", err)
	}

	timeout, err := optionalDuration(v, "backend.timeout")
	if err != nil {
		return nil, err
	}
	connectTimeout, err := optionalDuration(v, "backend.connect_timeout")
	if err != nil {
		return nil, err
	}

	maxConcurrent, err := optionalCount(v, "backend.max_concurrent")
	if err != nil {
		return nil, err
	}
	maxIDs, err := optionalCount(v, "backend.max_ids")
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		Server: ServerConfig{Port: v.GetInt("server.port")},
		Auth:   AuthConfig{Token: v.GetString("auth.token")},
		Tumblebug: TumblebugConfig{
			BaseURL:          strings.TrimRight(v.GetString("tumblebug.base_url"), "/"),
			Username:         v.GetString("tumblebug.username"),
			Password:         v.GetString("tumblebug.password"),
			CredentialHolder: v.GetString("tumblebug.credential_holder"),
			Timeout:          v.GetDuration("tumblebug.timeout"),
		},
		DB: DBConfig{
			Host:           v.GetString("db.host"),
			Port:           v.GetInt("db.port"),
			Name:           v.GetString("db.name"),
			User:           v.GetString("db.user"),
			Password:       v.GetString("db.password"),
			ConnectTimeout: v.GetDuration("db.connect_timeout"),
			ReadTimeout:    v.GetDuration("db.read_timeout"),
			WriteTimeout:   v.GetDuration("db.write_timeout"),
		},
		Collector: CollectorConfig{
			TelemetryPort: v.GetInt("collector.telemetry_port"),
			StorageDir:    v.GetString("collector.storage_dir"),
		},
		Backend: BackendConfig{
			Provider:        strings.ToLower(strings.TrimSpace(v.GetString("backend.provider"))),
			QueryURLs:       splitURLs(v.GetString("backend.query_url")),
			WriteURLs:       splitURLs(v.GetString("backend.write_url")),
			Timeout:         timeout,
			ConnectTimeout:  connectTimeout,
			MaxConcurrent:   maxConcurrent,
			MaxIDs:          maxIDs,
			PartialResponse: v.GetBool("backend.partial_response"),
			QueryUsername:   v.GetString("backend.query_username"),
			QueryPassword:   v.GetString("backend.query_password"),
			WriteUsername:   v.GetString("backend.write_username"),
		},
	}
	if cfg.Backend.Provider != BackendThanos && cfg.Backend.Provider != BackendVictoria {
		return nil, fmt.Errorf("%s_BACKEND must be %q or %q, got %q", envPrefix, BackendThanos, BackendVictoria, cfg.Backend.Provider)
	}
	if (cfg.Backend.QueryUsername == "") != (cfg.Backend.QueryPassword == "") {
		return nil, fmt.Errorf("%s_BACKEND_QUERY_USERNAME and _QUERY_PASSWORD must be set together", envPrefix)
	}
	if cfg.Server.Port <= 0 || cfg.Server.Port > 65535 {
		return nil, fmt.Errorf("%s_SERVER_PORT must be between 1 and 65535, got %d", envPrefix, cfg.Server.Port)
	}
	if cfg.Collector.TelemetryPort < 0 || cfg.Collector.TelemetryPort > 65535 {
		return nil, fmt.Errorf("%s_COLLECTOR_TELEMETRY_PORT must be between 0 and 65535, got %d", envPrefix, cfg.Collector.TelemetryPort)
	}
	if cfg.DB.Host != "" && (cfg.DB.Port <= 0 || cfg.DB.Port > 65535) {
		return nil, fmt.Errorf("%s_DB_PORT must be between 1 and 65535, got %d", envPrefix, cfg.DB.Port)
	}
	return cfg, nil
}

// splitURLs reads a comma-separated list of addresses, dropping empty entries
// and trailing slashes.
func splitURLs(s string) []string {
	var out []string
	for _, u := range strings.Split(s, ",") {
		if u = strings.TrimRight(strings.TrimSpace(u), "/"); u != "" {
			out = append(out, u)
		}
	}
	return out
}

// optionalCount reads a count that may be left empty, which gives 0 for the
// caller's default. A value given must be a positive integer.
func optionalCount(v *viper.Viper, key string) (int, error) {
	env := envName(key)
	raw := strings.TrimSpace(v.GetString(key))
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q; leave it empty for the default", env, raw)
	}
	return n, nil
}

func envName(key string) string {
	return envPrefix + "_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
}

// optionalDuration reads a duration that may be left empty, which gives 0 for
// the caller's default. A value given must be positive: there is no setting
// for "no limit".
func optionalDuration(v *viper.Viper, key string) (time.Duration, error) {
	env := envName(key)
	raw := strings.TrimSpace(v.GetString(key))
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration such as 30s: %w", env, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %s; leave it empty for the default", env, raw)
	}
	return d, nil
}

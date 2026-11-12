// Command ai-o11y-server serves the AI-O11y monitoring control plane: it lists
// what to monitor, renders the configuration of the collectors that monitor it,
// and answers queries against the metrics backend. Samples go from the
// collectors to the backend and never through this service.
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/cloud-barista/ai-o11y/internal/config"
	"github.com/cloud-barista/ai-o11y/internal/monitoring"
	"github.com/cloud-barista/ai-o11y/internal/monitoring/backend/promapi"
	"github.com/cloud-barista/ai-o11y/internal/monitoring/backend/thanos"
	"github.com/cloud-barista/ai-o11y/internal/monitoring/backend/victoria"
	"github.com/cloud-barista/ai-o11y/internal/monitoring/catalog"
	"github.com/cloud-barista/ai-o11y/internal/monitoring/collector"
	"github.com/cloud-barista/ai-o11y/internal/monitoring/mapping"
	"github.com/cloud-barista/ai-o11y/internal/monitoring/query"
	"github.com/cloud-barista/ai-o11y/internal/monitoring/source/tumblebug"
	"github.com/cloud-barista/ai-o11y/internal/rest"
)

const shutdownTimeout = 10 * time.Second

// @title AI-MCMP AI-O11y Service (Prototype)
// @version 0.1.0
// @description Monitoring control plane for AI-MCMP: monitoring targets, collector configuration and metric queries over a Prometheus-family backend.
// @license.name Apache 2.0
// @license.url http://www.apache.org/licenses/LICENSE-2.0.html
// @BasePath /aio11y
// @securityDefinitions.apikey BearerAuth
// @in header
// @name Authorization
// @description "Bearer <token>". Required on every route but readiness and this documentation when AIO11Y_AUTH_TOKEN is set.
func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Error().Err(err).Msg("Failed to load configuration")
		os.Exit(1)
	}
	if cfg.Auth.Token == "" {
		log.Warn().Msg("AIO11Y_AUTH_TOKEN is not set: every route is open")
	}

	sources, err := newTargetSources(cfg)
	if err != nil {
		log.Error().Err(err).Msg("Failed to register target sources")
		os.Exit(1)
	}
	log.Info().Strs("targetSources", sources.Names()).Msg("Target sources registered")

	deps, err := newDeps(cfg, sources)
	if err != nil {
		log.Error().Err(err).Msg("Failed to set up monitoring")
		os.Exit(1)
	}

	server := rest.NewServer(cfg, deps)

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Start()
	}()
	rest.MarkReady()
	log.Info().Int("port", server.Port()).Str("basePath", rest.BasePath).Msg("AI-O11y server started")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if err != nil {
			log.Error().Err(err).Msg("Server stopped")
			os.Exit(1)
		}
	case sig := <-stop:
		log.Info().Str("signal", sig.String()).Msg("Shutting down")
	}

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Error().Err(err).Msg("Failed to shut down cleanly")
		os.Exit(1)
	}
}

// newTargetSources registers the target source providers the configuration
// enables. A provider without its settings is left out rather than failing
// startup, so the service runs with whatever inventories are reachable.
func newTargetSources(cfg *config.Config) (*monitoring.Registry[monitoring.TargetSource], error) {
	sources := monitoring.NewRegistry[monitoring.TargetSource]("target source")
	if cfg.Tumblebug.BaseURL == "" {
		log.Warn().Msg("AIO11Y_TUMBLEBUG_BASE_URL is not set: CB-Tumblebug nodes are not listed")
		return sources, nil
	}
	client := tumblebug.NewClient(tumblebug.Config{
		BaseURL:          cfg.Tumblebug.BaseURL,
		Username:         cfg.Tumblebug.Username,
		Password:         cfg.Tumblebug.Password,
		CredentialHolder: cfg.Tumblebug.CredentialHolder,
		Timeout:          cfg.Tumblebug.Timeout,
	})
	if err := sources.Register(tumblebug.ProviderName, tumblebug.NewSource(client)); err != nil {
		return nil, err
	}
	return sources, nil
}

// newDeps builds what the monitoring routes run on from what is configured:
// the target table needs the database, syncing it also the management
// platform, and queries and collector configuration the metrics backend.
func newDeps(cfg *config.Config, sources *monitoring.Registry[monitoring.TargetSource]) (rest.Deps, error) {
	cat, err := catalog.New(catalog.Builtin())
	if err != nil {
		return rest.Deps{}, fmt.Errorf("built-in metric catalog: %w", err)
	}
	deps := rest.Deps{
		Catalog:          cat,
		Collectors:       []monitoring.CollectorKind{collector.Hostmetrics{}, collector.NvidiaGPU{}, collector.AMDGPU{}},
		CollectorOptions: collector.Options{TelemetryPort: cfg.Collector.TelemetryPort, StorageDir: cfg.Collector.StorageDir},
	}
	if src, err := sources.Get(tumblebug.ProviderName); err == nil {
		deps.Source = src
	}

	if cfg.DB.Host == "" {
		log.Warn().Msg("AIO11Y_DB_HOST is not set: target routes and queries are unavailable")
	} else {
		db, err := mapping.OpenDB(mapping.DBConfig{
			Host: cfg.DB.Host, Port: cfg.DB.Port, Name: cfg.DB.Name, User: cfg.DB.User, Password: cfg.DB.Password,
			ConnectTimeout: cfg.DB.ConnectTimeout, ReadTimeout: cfg.DB.ReadTimeout, WriteTimeout: cfg.DB.WriteTimeout,
		})
		if err != nil {
			return rest.Deps{}, err
		}
		store := mapping.NewStore(db, tumblebug.ProviderName)
		deps.Targets = store
		if deps.Source != nil {
			deps.Syncer = mapping.NewSyncer(store, deps.Source, mapping.DefaultSyncPause)
		}
	}

	b, err := newBackend(cfg, query.PromQL{Catalog: cat})
	if err != nil {
		return rest.Deps{}, err
	}
	if b != nil {
		deps.Backend = b
	}
	return deps, nil
}

// newBackend builds the metrics backend AIO11Y_BACKEND selects. It returns nil
// when no query URL is set, leaving queries and collector configuration
// unavailable.
func newBackend(cfg *config.Config, renderer query.PromQL) (monitoring.Backend, error) {
	bc := cfg.Backend
	if len(bc.QueryURLs) == 0 {
		log.Warn().Msg("AIO11Y_BACKEND_QUERY_URL is not set: queries and collector configuration are unavailable")
		return nil, nil
	}
	if len(bc.WriteURLs) == 0 {
		log.Warn().Msg("AIO11Y_BACKEND_WRITE_URL is not set: collector configuration is unavailable")
	}
	write := make([]monitoring.Endpoint, len(bc.WriteURLs))
	for i, u := range bc.WriteURLs {
		write[i] = monitoring.Endpoint{URL: u}
		if bc.WriteUsername != "" {
			write[i].BasicAuth = &monitoring.BasicAuth{Username: bc.WriteUsername, PasswordEnv: collector.WritePasswordEnv}
		}
	}
	limits := query.ChunkLimits{MaxIDs: bc.MaxIDs, MaxConcurrent: bc.MaxConcurrent}
	var headers map[string]string
	switch {
	case bc.QueryUsername != "":
		headers = map[string]string{"Authorization": basicAuth(bc.QueryUsername, bc.QueryPassword)}
	case bc.QueryBearerToken != "":
		headers = map[string]string{"Authorization": "Bearer " + bc.QueryBearerToken}
	}
	tlsCfg := promapi.TLS{CAFile: bc.QueryTLSCAFile, InsecureSkipVerify: bc.QueryTLSInsecureSkipVerify}
	if tlsCfg.InsecureSkipVerify {
		log.Warn().Msg("AIO11Y_BACKEND_QUERY_TLS_INSECURE_SKIP_VERIFY is on: query addresses' certificates are not verified")
	}
	switch bc.Provider {
	case config.BackendVictoria:
		return victoria.New(victoria.Config{
			QueryURLs: bc.QueryURLs, Write: write, Timeout: bc.Timeout, ConnectTimeout: bc.ConnectTimeout, Limits: limits, Headers: headers,
			TLS: tlsCfg, PartialResponse: bc.PartialResponse,
		}, renderer)
	case config.BackendThanos:
		return thanos.New(thanos.Config{
			QueryURLs: bc.QueryURLs, Write: write, Timeout: bc.Timeout, ConnectTimeout: bc.ConnectTimeout, Limits: limits, Headers: headers,
			TLS: tlsCfg, PartialResponse: bc.PartialResponse,
		}, renderer)
	default:
		return nil, fmt.Errorf("unknown metrics backend %q", bc.Provider)
	}
}

// basicAuth is the Authorization header value of a basic auth account.
func basicAuth(username, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
}

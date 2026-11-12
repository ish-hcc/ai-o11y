// Package victoria is the Backend provider for VictoriaMetrics: collectors
// write to the configured OTLP endpoint and queries go to its Prometheus query
// API, directly or through a proxy such as vmauth.
package victoria

import (
	"context"
	"errors"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/cloud-barista/ai-o11y/internal/monitoring"
	"github.com/cloud-barista/ai-o11y/internal/monitoring/backend/promapi"
	"github.com/cloud-barista/ai-o11y/internal/monitoring/query"
)

// Defaults for the settings left zero.
const (
	// DefaultTimeout is VictoriaMetrics' own -search.maxQueryDuration
	// default; the server stops a query there, so waiting longer gains
	// nothing.
	DefaultTimeout = 30 * time.Second
	// DefaultMaxIDs keeps the chunk size the Thanos backend uses. A chunk's
	// selector must also fit -search.maxQueryLen (16 KB by default) together
	// with the rest of the expression.
	DefaultMaxIDs = 255
	// DefaultMaxConcurrent is half of -search.maxConcurrentRequests on a
	// 4-vCPU server (twice the CPU count, so 8), leaving the other half to
	// every other user.
	DefaultMaxConcurrent = 4
)

// Config is how this deployment's VictoriaMetrics is reached.
type Config struct {
	// QueryURLs are addresses serving the Prometheus query API, such as
	// "http://vmauth:8427", in order of preference; a query moves to the next
	// when one cannot be reached or answers with a 5xx.
	QueryURLs []string
	// Write is where collectors send samples (OTLP over HTTP), in order of
	// preference, such as vmauths in front of the storage. VictoriaMetrics
	// keeps the names collectors send by default, which the metric catalog
	// queries; -opentelemetry.usePrometheusNaming must stay off.
	Write   []monitoring.Endpoint
	Timeout time.Duration
	// ConnectTimeout bounds connecting to one query address; zero takes
	// promapi.DefaultConnectTimeout.
	ConnectTimeout time.Duration
	// PartialResponse is not sent. Single-node VictoriaMetrics answers from
	// its one store; a cluster's vmselect answers partially by default unless
	// started with -search.denyPartialResponse, and that flag decides. It is
	// accepted and ignored.
	PartialResponse bool
	Limits          query.ChunkLimits
	// Headers are set on every query request.
	Headers map[string]string
	// TLS is how https query addresses are verified.
	TLS promapi.TLS
}

// Backend implements monitoring.Backend over the VictoriaMetrics query API.
// Deduplication of replicated samples is a server setting
// (-dedup.minScrapeInterval), so no query parameter asks for it.
type Backend struct {
	write   []monitoring.Endpoint
	client  *promapi.Client
	planner query.Planner
}

var _ monitoring.Backend = (*Backend)(nil)

// New builds the backend; renderer turns a QuerySpec into PromQL, which
// MetricsQL accepts.
func New(cfg Config, renderer query.PromQL) (*Backend, error) {
	if len(cfg.QueryURLs) == 0 {
		return nil, errors.New("victoria query URL is empty")
	}
	if cfg.PartialResponse {
		log.Warn().Msg("Partial responses are not sent to VictoriaMetrics: a single node has one store and a cluster's vmselect follows its -search.denyPartialResponse flag; the setting is ignored")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.Limits.MaxIDs == 0 {
		cfg.Limits.MaxIDs = DefaultMaxIDs
	}
	if cfg.Limits.MaxConcurrent == 0 {
		cfg.Limits.MaxConcurrent = DefaultMaxConcurrent
	}
	write := make([]monitoring.Endpoint, len(cfg.Write))
	for i, e := range cfg.Write {
		if e.Protocol == "" {
			e.Protocol = "otlp_http"
		}
		write[i] = e
	}
	client, err := promapi.NewClient(promapi.Config{
		ConnectTimeout: cfg.ConnectTimeout,
		BaseURLs:       cfg.QueryURLs,
		Timeout:        cfg.Timeout,
		Headers:        cfg.Headers,
		TLS:            cfg.TLS,
	})
	if err != nil {
		return nil, err
	}
	return &Backend{
		write:   write,
		client:  client,
		planner: query.Planner{Renderer: renderer, Limits: cfg.Limits, Runner: client},
	}, nil
}

// WriteEndpoints implements monitoring.Backend.
func (b *Backend) WriteEndpoints() []monitoring.Endpoint {
	return append([]monitoring.Endpoint(nil), b.write...)
}

// Query implements monitoring.Backend.
func (b *Backend) Query(ctx context.Context, q monitoring.QuerySpec) ([]monitoring.Series, error) {
	return b.planner.Query(ctx, q)
}

// Series implements monitoring.Backend.
func (b *Backend) Series(ctx context.Context, match string) ([]map[string]string, error) {
	return b.client.Series(ctx, match)
}

// Capabilities implements monitoring.Backend.
func (b *Backend) Capabilities() monitoring.BackendCaps {
	return monitoring.BackendCaps{"promql": true}
}

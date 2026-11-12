// Package thanos is the Backend provider for Thanos: collectors write to the
// configured OTLP endpoint and queries go to the Thanos Querier.
package thanos

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"time"

	"github.com/cloud-barista/ai-o11y/internal/monitoring"
	"github.com/cloud-barista/ai-o11y/internal/monitoring/backend/promapi"
	"github.com/cloud-barista/ai-o11y/internal/monitoring/query"
)

// Defaults for the settings left zero.
const (
	// DefaultTimeout is the Thanos Querier's own --query.timeout default.
	DefaultTimeout = 2 * time.Minute
	// DefaultMaxIDs keeps a chunk in the 255-alternative regex set lookup
	// Prometheus and Thanos match through.
	DefaultMaxIDs = 255
	// DefaultMaxConcurrent stays at half the Thanos Querier's
	// --query.max-concurrent default of 20, which every other user shares.
	DefaultMaxConcurrent = 10
)

// Config is how this deployment's Thanos is reached.
type Config struct {
	// QueryURLs are Thanos Querier HTTP addresses, such as
	// "http://thanos-query:10902", in order of preference; a query moves to
	// the next when one cannot be reached or answers with a 5xx.
	QueryURLs []string
	// Write is where collectors send samples (OTLP over HTTP), in order of
	// preference. With a Sidecar it is Prometheus' OTLP receiver, which must
	// keep the names collectors send (otlp.translation_strategy:
	// NoTranslation); the metric catalog queries those names.
	Write []monitoring.Endpoint
	// PartialResponse lets a query answer with what the reachable stores hold
	// when another store fails. Off, any failing store fails the query, even
	// a replica whose data another one also holds. It is off for now, a
	// decision to revisit with the deployment's sharding and replication.
	PartialResponse bool
	Timeout         time.Duration
	// ConnectTimeout bounds connecting to one query address; zero takes
	// promapi.DefaultConnectTimeout.
	ConnectTimeout time.Duration
	Limits         query.ChunkLimits
	// Headers are set on every query request.
	Headers map[string]string
	// TLS is how https query addresses are verified.
	TLS promapi.TLS
}

// Backend implements monitoring.Backend over a Thanos Querier.
type Backend struct {
	write   []monitoring.Endpoint
	client  *promapi.Client
	planner query.Planner
}

var _ monitoring.Backend = (*Backend)(nil)

// New builds the backend; renderer turns a QuerySpec into PromQL.
func New(cfg Config, renderer query.PromQL) (*Backend, error) {
	if len(cfg.QueryURLs) == 0 {
		return nil, errors.New("thanos query URL is empty")
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
		Params: url.Values{
			// The Querier deduplicates by default; it is sent so a querier
			// started otherwise still merges replicas.
			"dedup":            {"true"},
			"partial_response": {strconv.FormatBool(cfg.PartialResponse)},
		},
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

// Package promapi is a client for the Prometheus HTTP query API, which
// Prometheus, the Thanos Querier, Mimir and VictoriaMetrics all serve. Backend
// providers build on it and add what is their own, such as Thanos' dedup
// parameter.
package promapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/cloud-barista/ai-o11y/internal/monitoring"
	"github.com/cloud-barista/ai-o11y/internal/retry"
)

const (
	maxErrorBody = 4 << 10
	// httpSlack is how much longer the client waits than the query timeout
	// it sends, so the backend's own timeout error arrives first.
	httpSlack = 5 * time.Second
	// DefaultConnectTimeout bounds connecting to one address, so an address
	// that does not answer at all hands over to the next one quickly.
	DefaultConnectTimeout = 5 * time.Second
)

// Config is where the query API is and how every request is sent.
type Config struct {
	// BaseURLs are the addresses the /api/v1 paths hang from, such as
	// "http://thanos-query:10902" or "https://vm-a:8443/metrics" behind a
	// proxy that serves the API under a path, in order of preference. Every request
	// starts at the first; it moves to the next when an address cannot be
	// reached or answers with a 5xx. Each address serves the same data, such
	// as two queriers or two query proxies in front of replicated storage.
	BaseURLs []string
	// Timeout is sent as the timeout parameter of every query, so time spent
	// waiting for a query slot counts against it too. It also bounds one
	// request across every address it is tried at, so a slow address leaves
	// the next one only what remains.
	Timeout time.Duration
	// ConnectTimeout bounds connecting to one address; zero takes
	// DefaultConnectTimeout.
	ConnectTimeout time.Duration
	// Headers are set on every request.
	Headers map[string]string
	// Params are added to every query, such as Thanos' dedup.
	Params url.Values
	// TLS is how https addresses are verified.
	TLS TLS
}

// TLS is how the certificates of https query addresses are verified. The zero
// value verifies them against the system's CAs.
type TLS struct {
	// CAFile is a PEM file of the CAs to verify against instead of the
	// system's, such as a private CA.
	CAFile string
	// InsecureSkipVerify accepts any certificate. It is for trying out a
	// setup only: anyone on the path can then read and answer queries.
	InsecureSkipVerify bool
}

// config builds the client TLS configuration; nil keeps Go's default.
func (t TLS) config() (*tls.Config, error) {
	if t.CAFile == "" && !t.InsecureSkipVerify {
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: t.InsecureSkipVerify}
	if t.CAFile != "" {
		pem, err := os.ReadFile(t.CAFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read query CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("query CA file %s holds no PEM certificate", t.CAFile)
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

// Client sends PromQL to the query API. Every query is a form POST: a query
// over 255 entity ids is about 9.4 KB, over the 8 KB request line many
// proxies allow.
type Client struct {
	cfg        Config
	baseURLs   []string
	httpClient *http.Client
	retry      retry.Policy
}

// NewClient builds a Client from cfg.
func NewClient(cfg Config) (*Client, error) {
	if len(cfg.BaseURLs) == 0 {
		return nil, errors.New("query API base URL is empty")
	}
	baseURLs := make([]string, 0, len(cfg.BaseURLs))
	for _, u := range cfg.BaseURLs {
		u = strings.TrimRight(strings.TrimSpace(u), "/")
		if u == "" {
			return nil, errors.New("query API base URL is empty")
		}
		baseURLs = append(baseURLs, u)
	}
	if cfg.Timeout <= 0 {
		return nil, errors.New("query timeout must be positive")
	}
	if cfg.ConnectTimeout < 0 {
		return nil, errors.New("connect timeout must not be negative")
	}
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = DefaultConnectTimeout
	}
	tlsConfig, err := cfg.TLS.config()
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: cfg.ConnectTimeout, KeepAlive: 30 * time.Second}).DialContext
	if tlsConfig != nil {
		transport.TLSClientConfig = tlsConfig
	}
	return &Client{
		cfg:        cfg,
		baseURLs:   baseURLs,
		httpClient: &http.Client{Transport: transport},
		retry:      retry.Default(),
	}, nil
}

// Instant evaluates expr at at; a zero at means now.
func (c *Client) Instant(ctx context.Context, expr string, at time.Time) ([]monitoring.Series, error) {
	form := url.Values{"query": {expr}}
	if !at.IsZero() {
		form.Set("time", formatTime(at))
	}
	return c.query(ctx, "/api/v1/query", form)
}

// Range evaluates expr from start to end every step.
func (c *Client) Range(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]monitoring.Series, error) {
	if step <= 0 {
		return nil, errors.New("range query needs a positive step")
	}
	form := url.Values{
		"query": {expr},
		"start": {formatTime(start)},
		"end":   {formatTime(end)},
		"step":  {strconv.FormatFloat(step.Seconds(), 'f', -1, 64)},
	}
	return c.query(ctx, "/api/v1/query_range", form)
}

// Series returns the label sets of the series match selects.
func (c *Client) Series(ctx context.Context, match string) ([]map[string]string, error) {
	var out []map[string]string
	if err := c.post(ctx, "/api/v1/series", url.Values{"match[]": {match}}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) query(ctx context.Context, path string, form url.Values) ([]monitoring.Series, error) {
	// Seconds: Prometheus does not read Go's "1.5s" or "2m0s" forms.
	form.Set("timeout", strconv.FormatFloat(c.cfg.Timeout.Seconds(), 'f', -1, 64))
	var data queryData
	if err := c.post(ctx, path, form, &data); err != nil {
		return nil, err
	}
	return data.series()
}

// post sends form to path with the configured parameters and decodes the data
// of a successful answer into out. A 429 is retried with backoff
// (internal/retry); queries are reads, so sending one again is safe. The
// timeout bounds the whole request: retries and every address included.
func (c *Client) post(ctx context.Context, path string, form url.Values, out any) error {
	for name, values := range c.cfg.Params {
		form[name] = values
	}
	body := form.Encode()
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout+httpSlack)
	defer cancel()
	return c.retry.Do(ctx, func() (bool, time.Duration, error) {
		after, err := c.postAny(ctx, path, body, out)
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusTooManyRequests {
			log.Debug().Str("path", path).Msg("Query API rate-limited the request")
			return true, after, err
		}
		return false, 0, err
	})
}

// postAny sends the request to each address in turn until one answers with
// something other than a failure to connect or a 5xx. A 4xx is the query's
// own fault and would be the same at every address, so it is returned at
// once, as is a 429, which post retries from the first address.
func (c *Client) postAny(ctx context.Context, path, body string, out any) (time.Duration, error) {
	var (
		after time.Duration
		err   error
	)
	for i, base := range c.baseURLs {
		after, err = c.postOnce(ctx, base, path, body, out)
		if !failsOver(err) || ctx.Err() != nil {
			return after, err
		}
		if i+1 < len(c.baseURLs) {
			log.Warn().Err(err).Str("path", path).Str("next", redact(c.baseURLs[i+1])).Msg("Query API address failed, trying the next one")
		}
	}
	return after, err
}

// redact hides the password of an address with user info, for logs.
func redact(addr string) string {
	u, err := url.Parse(addr)
	if err != nil {
		return "(unparsable address)"
	}
	return u.Redacted()
}

// failsOver tells whether err is worth sending the request to another address.
func failsOver(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrUnavailable) {
		return true
	}
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode >= 500
}

func (c *Client) postOnce(ctx context.Context, base, path, body string, out any) (time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, strings.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("failed to build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	for name, value := range c.cfg.Headers {
		req.Header.Set(name, value)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%w: %s: %w", ErrUnavailable, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("failed to read query API response for %s: %w", path, err)
	}
	var env envelope
	jsonErr := json.Unmarshal(raw, &env)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || env.Status != "success" {
		apiErr := &APIError{StatusCode: resp.StatusCode, Type: env.ErrorType, Message: env.Error}
		if jsonErr != nil || apiErr.Message == "" {
			apiErr.Message = strings.TrimSpace(string(raw[:min(len(raw), maxErrorBody)]))
		}
		return retry.After(resp.Header), apiErr
	}
	if jsonErr != nil {
		return 0, fmt.Errorf("failed to decode query API response for %s: %w", path, jsonErr)
	}
	for _, w := range env.Warnings {
		log.Warn().Str("path", path).Str("warning", w).Msg("Query API answered with a warning")
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return 0, fmt.Errorf("failed to decode query API data for %s: %w", path, err)
	}
	return 0, nil
}

// ErrUnavailable wraps a failure to reach the query API at all.
var ErrUnavailable = errors.New("query API unreachable")

// APIError is an answer the query API did not mark as a success.
type APIError struct {
	StatusCode int
	// Type is the API's errorType, such as "bad_data" or "timeout".
	Type    string
	Message string
}

func (e *APIError) Error() string {
	if e.Type != "" {
		return fmt.Sprintf("query API responded %d (%s): %s", e.StatusCode, e.Type, e.Message)
	}
	return fmt.Sprintf("query API responded %d: %s", e.StatusCode, e.Message)
}

// envelope is the frame of every query API answer.
type envelope struct {
	Status    string          `json:"status"`
	Data      json.RawMessage `json:"data"`
	ErrorType string          `json:"errorType"`
	Error     string          `json:"error"`
	Warnings  []string        `json:"warnings"`
}

type queryData struct {
	ResultType string          `json:"resultType"`
	Result     json.RawMessage `json:"result"`
}

type sample [2]json.RawMessage

type sampleSeries struct {
	Metric map[string]string `json:"metric"`
	Value  *sample           `json:"value"`
	Values []sample          `json:"values"`
}

func (d queryData) series() ([]monitoring.Series, error) {
	switch d.ResultType {
	case "vector", "matrix":
		var raw []sampleSeries
		if err := json.Unmarshal(d.Result, &raw); err != nil {
			return nil, fmt.Errorf("failed to decode %s result: %w", d.ResultType, err)
		}
		out := make([]monitoring.Series, 0, len(raw))
		for _, r := range raw {
			s := monitoring.Series{Labels: r.Metric}
			if s.Labels == nil {
				s.Labels = map[string]string{}
			}
			samples := r.Values
			if r.Value != nil {
				samples = []sample{*r.Value}
			}
			for _, smp := range samples {
				p, err := smp.point()
				if err != nil {
					return nil, err
				}
				s.Points = append(s.Points, p)
			}
			out = append(out, s)
		}
		return out, nil
	case "scalar":
		var smp sample
		if err := json.Unmarshal(d.Result, &smp); err != nil {
			return nil, fmt.Errorf("failed to decode scalar result: %w", err)
		}
		p, err := smp.point()
		if err != nil {
			return nil, err
		}
		return []monitoring.Series{{Labels: map[string]string{}, Points: []monitoring.Point{p}}}, nil
	default:
		return nil, fmt.Errorf("unsupported result type %q", d.ResultType)
	}
}

// point decodes [unix seconds, "value"]; the value may be "NaN" or "+Inf".
func (s sample) point() (monitoring.Point, error) {
	var ts float64
	if err := json.Unmarshal(s[0], &ts); err != nil {
		return monitoring.Point{}, fmt.Errorf("bad sample time %s: %w", s[0], err)
	}
	var str string
	if err := json.Unmarshal(s[1], &str); err != nil {
		return monitoring.Point{}, fmt.Errorf("bad sample value %s: %w", s[1], err)
	}
	v, err := strconv.ParseFloat(str, 64)
	if err != nil {
		return monitoring.Point{}, fmt.Errorf("bad sample value %q: %w", str, err)
	}
	sec, frac := math.Modf(ts)
	t := time.Unix(int64(sec), int64(math.Round(frac*1e3))*int64(time.Millisecond)).UTC()
	return monitoring.Point{T: t, V: v}, nil
}

func formatTime(t time.Time) string {
	return strconv.FormatFloat(float64(t.UnixMilli())/1e3, 'f', -1, 64)
}

// Package tumblebug is the TargetSource provider for CB-Tumblebug: it lists the
// nodes CB-Tumblebug runs as monitoring targets.
package tumblebug

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/cloud-barista/ai-o11y/internal/retry"
)

const (
	headerCredentialHolder = "x-credential-holder"
	maxErrorBodyBytes      = 4 << 10
)

// Config is where CB-Tumblebug is reached and how.
type Config struct {
	// BaseURL includes the /tumblebug prefix, such as
	// "http://localhost:1323/tumblebug".
	BaseURL          string
	Username         string
	Password         string
	CredentialHolder string
	Timeout          time.Duration
}

// Client calls the CB-Tumblebug REST API. Only the read calls monitoring needs
// are here.
type Client struct {
	baseURL          string
	username         string
	password         string
	credentialHolder string
	httpClient       *http.Client
	retry            retry.Policy
}

// NewClient builds a Client from configuration.
func NewClient(cfg Config) *Client {
	return &Client{
		baseURL:          strings.TrimRight(cfg.BaseURL, "/"),
		username:         cfg.Username,
		password:         cfg.Password,
		credentialHolder: cfg.CredentialHolder,
		httpClient:       &http.Client{Timeout: cfg.Timeout},
		retry:            retry.Default(),
	}
}

// APIError is a non-2xx response from CB-Tumblebug.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("tumblebug responded %d: %s", e.StatusCode, e.Message)
}

// StatusCode reports the upstream HTTP status for err, or 0 when err is not an APIError.
func StatusCode(err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode
	}
	return 0
}

// get sends a GET request to CB-Tumblebug and decodes the JSON response into
// out. A 429 is retried with backoff (internal/retry); the last 429 is
// returned as an APIError.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	return c.retry.Do(ctx, func() (bool, time.Duration, error) {
		after, err := c.getOnce(ctx, path, query, out)
		if StatusCode(err) == http.StatusTooManyRequests {
			log.Debug().Str("path", path).Msg("CB-Tumblebug rate-limited the request")
			return true, after, err
		}
		return false, 0, err
	})
}

// getOnce sends one GET request. On a 429 it also returns the wait
// CB-Tumblebug named in Retry-After, if any.
func (c *Client) getOnce(ctx context.Context, path string, query url.Values, out any) (time.Duration, error) {
	endpoint := c.baseURL + "/" + strings.TrimLeft(path, "/")
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(c.username, c.password)
	if c.credentialHolder != "" {
		req.Header.Set(headerCredentialHolder, c.credentialHolder)
	}

	// Path only: query values may carry operational detail.
	log.Debug().Str("path", path).Msg("Calling CB-Tumblebug")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("failed to call tumblebug GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return retry.After(resp.Header), &APIError{StatusCode: resp.StatusCode, Message: readErrorMessage(resp.Body)}
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return 0, fmt.Errorf("failed to decode tumblebug response for GET %s: %w", path, err)
	}
	return 0, nil
}

// readErrorMessage extracts the Tumblebug SimpleMsg message, falling back to raw text.
func readErrorMessage(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, maxErrorBodyBytes))
	if err != nil {
		return "no response body"
	}
	var simple struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &simple) == nil && simple.Message != "" {
		return simple.Message
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return "empty response body"
	}
	return trimmed
}

// namespace is the part of a CB-Tumblebug namespace monitoring reads.
type namespace struct {
	ID string `json:"id"`
}

// node is the part of a CB-Tumblebug node (model.NodeInfo) monitoring reads.
// Only these fields are decoded, so fields CB-Tumblebug adds or renames
// elsewhere do not matter.
type node struct {
	InfraID          string `json:"infraId"`
	ID               string `json:"id"`
	UID              string `json:"uid"`
	Name             string `json:"name"`
	CSPResourceID    string `json:"cspResourceId"`
	ConnectionName   string `json:"connectionName"`
	ConnectionConfig struct {
		ProviderName string `json:"providerName"`
	} `json:"connectionConfig"`
	Region struct {
		Region string `json:"region"`
		Zone   string `json:"zone"`
	} `json:"region"`
	SpecID string      `json:"specId"`
	Spec   accelerator `json:"spec"`
	Image  struct {
		OSType         string `json:"osType"`
		OSDistribution string `json:"osDistribution"`
	} `json:"image"`
}

// accelerator is the part of a CB-Tumblebug spec (model.SpecInfo) monitoring
// reads. A node carries it under "spec", but CB-Tumblebug (v0.13.8) builds that
// from a node group summary without the accelerator fields, so a node's spec
// is usually empty here and the spec itself has to be read by the node's
// specId. acceleratorCount is null for some specs, such as Azure
// Standard_NV4ads_V710_v5, and then decodes to 0.
type accelerator struct {
	AcceleratorType  string `json:"acceleratorType"`
	AcceleratorModel string `json:"acceleratorModel"`
	AcceleratorCount int    `json:"acceleratorCount"`
}

// specNamespace is the namespace CB-Tumblebug keeps the specs of every CSP in
// (model.SystemCommonNs); a node's specId names a spec there.
const specNamespace = "system"

// listNamespaces returns every namespace (GET /ns).
func (c *Client) listNamespaces(ctx context.Context) ([]namespace, error) {
	var out struct {
		Ns []namespace `json:"ns"`
	}
	if err := c.get(ctx, "/ns", nil, &out); err != nil {
		return nil, err
	}
	return out.Ns, nil
}

// listNodes returns every node in a namespace across its infras
// (GET /ns/{nsId}/resources/node).
func (c *Client) listNodes(ctx context.Context, nsID string) ([]node, error) {
	var out struct {
		Node []node `json:"node"`
	}
	path := fmt.Sprintf("/ns/%s/resources/node", url.PathEscape(nsID))
	if err := c.get(ctx, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Node, nil
}

// getNode returns one node (GET /ns/{nsId}/infra/{infraId}/node/{nodeId}).
func (c *Client) getNode(ctx context.Context, nsID, infraID, nodeID string) (node, error) {
	var out node
	path := fmt.Sprintf("/ns/%s/infra/%s/node/%s",
		url.PathEscape(nsID), url.PathEscape(infraID), url.PathEscape(nodeID))
	err := c.get(ctx, path, nil, &out)
	return out, err
}

// getSpec returns the accelerator of a spec
// (GET /ns/system/resources/spec/{specId}).
func (c *Client) getSpec(ctx context.Context, specID string) (accelerator, error) {
	var out accelerator
	path := fmt.Sprintf("/ns/%s/resources/spec/%s", specNamespace, url.PathEscape(specID))
	err := c.get(ctx, path, nil, &out)
	return out, err
}

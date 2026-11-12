package main

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloud-barista/ai-o11y/internal/config"
	"github.com/cloud-barista/ai-o11y/internal/monitoring/query"
)

// TestBackendQueryAuthAndTLS checks each provider sends the configured
// account over https verified with the configured CA, under the address's
// path prefix.
func TestBackendQueryAuthAndTLS(t *testing.T) {
	var path, auth string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"status":"success","data":[]}`))
	}))
	defer srv.Close()
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		backend  config.BackendConfig
		wantAuth string
	}{
		{"bearer", config.BackendConfig{QueryBearerToken: "read-token"}, "Bearer read-token"},
		{"basic", config.BackendConfig{QueryUsername: "reader", QueryPassword: "pw"}, "Basic cmVhZGVyOnB3"},
		{"none", config.BackendConfig{}, ""},
	}
	for _, provider := range []string{config.BackendThanos, config.BackendVictoria} {
		for _, c := range cases {
			bc := c.backend
			bc.Provider = provider
			bc.QueryURLs = []string{srv.URL + "/metrics"}
			bc.QueryTLSCAFile = caFile
			b, err := newBackend(&config.Config{Backend: bc}, query.PromQL{})
			if err != nil {
				t.Fatalf("%s %s: newBackend: %v", provider, c.name, err)
			}
			path, auth = "", ""
			if _, err := b.Series(context.Background(), "up"); err != nil {
				t.Fatalf("%s %s: Series: %v", provider, c.name, err)
			}
			if path != "/metrics/api/v1/series" || auth != c.wantAuth {
				t.Errorf("%s %s: path %q, Authorization %q, want /metrics/api/v1/series and %q", provider, c.name, path, auth, c.wantAuth)
			}
		}
	}
}

func TestBackendUnreadableCAFails(t *testing.T) {
	bc := config.BackendConfig{
		Provider:       config.BackendVictoria,
		QueryURLs:      []string{"https://q:8443/metrics"},
		QueryTLSCAFile: filepath.Join(t.TempDir(), "absent.pem"),
	}
	if _, err := newBackend(&config.Config{Backend: bc}, query.PromQL{}); err == nil {
		t.Error("newBackend with a missing CA file succeeded")
	}
}

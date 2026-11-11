package tumblebug

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/cloud-barista/ai-o11y/internal/monitoring"
)

// fakeTumblebug serves the two listing routes with fixed bodies and records
// what the client sent.
func fakeTumblebug(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var paths []string
	mux := http.NewServeMux()
	check := func(r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "admin" || pass != "secret" {
			t.Errorf("%s: basic auth = %q/%q, %v", r.URL.Path, user, pass, ok)
		}
		if got := r.Header.Get(headerCredentialHolder); got != "holder01" {
			t.Errorf("%s: credential holder = %q", r.URL.Path, got)
		}
		paths = append(paths, r.URL.Path)
	}
	mux.HandleFunc("GET /tumblebug/ns", func(w http.ResponseWriter, r *http.Request) {
		check(r)
		_, _ = w.Write([]byte(`{"ns":[{"id":"ns01","name":"ns01"},{"id":"ns02","name":"ns02"}]}`))
	})
	mux.HandleFunc("GET /tumblebug/ns/ns01/resources/node", func(w http.ResponseWriter, r *http.Request) {
		check(r)
		_, _ = w.Write([]byte(`{"node":[{"infraId":"infra01","id":"node01","uid":"d44ikdomt0tac4ejrmmg","name":"web-1",
			"cspResourceId":"i-0d6c7d2a0f4d0ba2a","connectionName":"aws-ap-northeast-2",
			"connectionConfig":{"providerName":"aws"},"region":{"region":"ap-northeast-2","zone":"ap-northeast-2a"},
			"spec":{"acceleratorType":"GPU","acceleratorModel":"NVIDIA T4","acceleratorCount":1},
			"image":{"osType":"ubuntu 22.04"}}]}`))
	})
	mux.HandleFunc("GET /tumblebug/ns/ns02/resources/node", func(w http.ResponseWriter, r *http.Request) {
		check(r)
		_, _ = w.Write([]byte(`{"node":[{"infraId":"infra09","id":"node09","uid":"c9nqt72b9s0d3f6qe2cg"},
			{"infraId":"infra09","id":"half-made"}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &paths
}

func newTestSource(baseURL string) *Source {
	return NewSource(NewClient(Config{
		BaseURL:          baseURL + "/tumblebug",
		Username:         "admin",
		Password:         "secret",
		CredentialHolder: "holder01",
		Timeout:          5 * time.Second,
	}))
}

func TestListAllNamespaces(t *testing.T) {
	srv, _ := fakeTumblebug(t)

	got, err := newTestSource(srv.URL).List(context.Background(), nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []monitoring.Target{
		{
			EntityID: "d44ikdomt0tac4ejrmmg",
			Kind:     KindVM,
			Name:     "web-1",
			Provider: "aws",
			Region:   "ap-northeast-2",
			OS:       "linux",
			GPUCount: 1,
			GPUModel: "NVIDIA T4",
			Extra: map[string]string{
				ExtraNamespace:     "ns01",
				ExtraInfra:         "infra01",
				ExtraNode:          "node01",
				ExtraConnection:    "aws-ap-northeast-2",
				ExtraZone:          "ap-northeast-2a",
				ExtraCSPResourceID: "i-0d6c7d2a0f4d0ba2a",
				ExtraOSType:        "ubuntu 22.04",
			},
		},
		{
			// A node with only the fields it must have: no name, GPU or OS
			// is made up for it.
			EntityID: "c9nqt72b9s0d3f6qe2cg",
			Kind:     KindVM,
			Extra: map[string]string{
				ExtraNamespace: "ns02",
				ExtraInfra:     "infra09",
				ExtraNode:      "node09",
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("List =\n%+v\nwant\n%+v", got, want)
	}
}

// specTumblebug lists nodes as CB-Tumblebug v0.13.8 does, with a spec that
// names no accelerator and a specId, and serves the specs from the system
// namespace. The bodies are trimmed from real answers for Azure GPU VMs. It
// counts the requests per path.
func specTumblebug(t *testing.T) (*httptest.Server, map[string]int) {
	t.Helper()
	calls := map[string]int{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /tumblebug/", func(w http.ResponseWriter, r *http.Request) {
		calls[r.URL.Path]++
		switch r.URL.Path {
		case "/tumblebug/ns/ns01/resources/node":
			_, _ = w.Write([]byte(`{"node":[
				{"infraId":"infra01","id":"t4-1","uid":"uid-t4-1","specId":"azure+koreacentral+standard_nc4as_t4_v3",
				 "spec":{"cspSpecName":"Standard_NC4as_T4_v3","vCPU":4,"memoryGiB":28,"costPerHour":0.647}},
				{"infraId":"infra01","id":"t4-2","uid":"uid-t4-2","specId":"azure+koreacentral+standard_nc4as_t4_v3",
				 "spec":{"cspSpecName":"Standard_NC4as_T4_v3","vCPU":4,"memoryGiB":28,"costPerHour":0.647}},
				{"infraId":"infra02","id":"v710-1","uid":"uid-v710-1","specId":"azure+eastus+standard_nv4ads_v710_v5",
				 "spec":{"cspSpecName":"Standard_NV4ads_V710_v5","vCPU":4,"memoryGiB":16}},
				{"infraId":"infra03","id":"broken-1","uid":"uid-broken-1","specId":"azure+eastus+broken",
				 "spec":{"cspSpecName":"Broken","vCPU":2,"memoryGiB":8}},
				{"infraId":"infra03","id":"broken-2","uid":"uid-broken-2","specId":"azure+eastus+broken",
				 "spec":{"cspSpecName":"Broken","vCPU":2,"memoryGiB":8}},
				{"infraId":"infra04","id":"cpu-1","uid":"uid-cpu-1","specId":"azure+koreacentral+standard_d2s_v5",
				 "spec":{"cspSpecName":"Standard_D2s_v5","vCPU":2,"memoryGiB":8}},
				{"infraId":"infra05","id":"inline-1","uid":"uid-inline-1","specId":"aws+ap-northeast-2+g4dn.xlarge",
				 "spec":{"acceleratorType":"gpu","acceleratorModel":"NVIDIA T4","acceleratorCount":1}}]}`))
		case "/tumblebug/ns/system/resources/spec/azure+koreacentral+standard_nc4as_t4_v3":
			_, _ = w.Write([]byte(`{"id":"azure+koreacentral+standard_nc4as_t4_v3","cspSpecName":"Standard_NC4as_T4_v3",
				"vCPU":4,"memoryGiB":28,"acceleratorType":"gpu","acceleratorModel":"NVIDIA Tesla T4",
				"acceleratorCount":1,"acceleratorMemoryGB":16}`))
		case "/tumblebug/ns/system/resources/spec/azure+eastus+standard_nv4ads_v710_v5":
			_, _ = w.Write([]byte(`{"id":"azure+eastus+standard_nv4ads_v710_v5","cspSpecName":"Standard_NV4ads_V710_v5",
				"vCPU":4,"memoryGiB":16,"acceleratorType":"gpu","acceleratorModel":"AMD RADEON PRO V710",
				"acceleratorCount":null,"acceleratorMemoryGB":24}`))
		case "/tumblebug/ns/system/resources/spec/azure+koreacentral+standard_d2s_v5":
			_, _ = w.Write([]byte(`{"id":"azure+koreacentral+standard_d2s_v5","cspSpecName":"Standard_D2s_v5",
				"vCPU":2,"memoryGiB":8,"acceleratorType":"","acceleratorModel":"","acceleratorCount":0}`))
		case "/tumblebug/ns/ns01/infra/infra02/node/v710-1":
			_, _ = w.Write([]byte(`{"infraId":"infra02","id":"v710-1","uid":"uid-v710-1",
				"specId":"azure+eastus+standard_nv4ads_v710_v5",
				"spec":{"cspSpecName":"Standard_NV4ads_V710_v5","vCPU":4,"memoryGiB":16}}`))
		case "/tumblebug/ns/ns01/infra/infra03/node/broken-1":
			_, _ = w.Write([]byte(`{"infraId":"infra03","id":"broken-1","uid":"uid-broken-1","specId":"azure+eastus+broken",
				"spec":{"cspSpecName":"Broken","vCPU":2,"memoryGiB":8}}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"failed to get spec"}`))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, calls
}

// TestListReadsGPUFromSpec: a node whose own spec names no accelerator gets
// its GPUs from the spec its specId names, read once per specId; a spec that
// cannot be read leaves the node listed without GPUs.
func TestListReadsGPUFromSpec(t *testing.T) {
	srv, calls := specTumblebug(t)

	got, err := newTestSource(srv.URL).List(context.Background(), monitoring.Scope{ScopeNamespace: "ns01"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	type gpu struct {
		count int
		model string
	}
	gotGPU := map[string]gpu{}
	for _, target := range got {
		gotGPU[target.EntityID] = gpu{target.GPUCount, target.GPUModel}
	}
	wantGPU := map[string]gpu{
		"uid-t4-1":     {1, "NVIDIA Tesla T4"},
		"uid-t4-2":     {1, "NVIDIA Tesla T4"},
		"uid-v710-1":   {0, "AMD RADEON PRO V710"},
		"uid-broken-1": {},
		"uid-broken-2": {},
		"uid-cpu-1":    {},
		"uid-inline-1": {1, "NVIDIA T4"},
	}
	if !reflect.DeepEqual(gotGPU, wantGPU) {
		t.Errorf("GPUs = %+v, want %+v", gotGPU, wantGPU)
	}
	for path, want := range map[string]int{
		"/tumblebug/ns/system/resources/spec/azure+koreacentral+standard_nc4as_t4_v3": 1,
		"/tumblebug/ns/system/resources/spec/azure+eastus+standard_nv4ads_v710_v5":    1,
		"/tumblebug/ns/system/resources/spec/azure+koreacentral+standard_d2s_v5":      1,
		"/tumblebug/ns/system/resources/spec/azure+eastus+broken":                     1,
		"/tumblebug/ns/system/resources/spec/aws+ap-northeast-2+g4dn.xlarge":          0,
	} {
		if calls[path] != want {
			t.Errorf("%s called %d times, want %d", path, calls[path], want)
		}
	}
}

// TestFetchReadsGPUFromSpec: Fetch, which registration uses, reads the spec
// the same way, and registers the node without GPUs when the spec fails.
func TestFetchReadsGPUFromSpec(t *testing.T) {
	srv, _ := specTumblebug(t)
	s := newTestSource(srv.URL)

	got, err := s.Fetch(context.Background(), map[string]string{ExtraNamespace: "ns01", ExtraInfra: "infra02", ExtraNode: "v710-1"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.EntityID != "uid-v710-1" || got.GPUCount != 0 || got.GPUModel != "AMD RADEON PRO V710" {
		t.Errorf("Fetch = %+v, want the V710 model with no count", got)
	}

	got, err = s.Fetch(context.Background(), map[string]string{ExtraNamespace: "ns01", ExtraInfra: "infra03", ExtraNode: "broken-1"})
	if err != nil {
		t.Fatalf("Fetch with a failing spec: %v", err)
	}
	if got.EntityID != "uid-broken-1" || got.GPUCount != 0 || got.GPUModel != "" {
		t.Errorf("Fetch with a failing spec = %+v, want the node without GPUs", got)
	}
}

func TestListOneNamespace(t *testing.T) {
	srv, paths := fakeTumblebug(t)

	got, err := newTestSource(srv.URL).List(context.Background(), monitoring.Scope{ScopeNamespace: "ns02"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].EntityID != "c9nqt72b9s0d3f6qe2cg" {
		t.Fatalf("List = %+v, want only the node with a Uid", got)
	}
	if want := []string{"/tumblebug/ns/ns02/resources/node"}; !reflect.DeepEqual(*paths, want) {
		t.Fatalf("paths = %v, want %v (no namespace listing)", *paths, want)
	}
}

func TestListReportsTumblebugError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"namespace ns404 does not exist"}`))
	}))
	t.Cleanup(srv.Close)

	_, err := newTestSource(srv.URL).List(context.Background(), monitoring.Scope{ScopeNamespace: "ns404"})
	if StatusCode(err) != http.StatusNotFound {
		t.Fatalf("StatusCode(%v) = %d, want 404", err, StatusCode(err))
	}
}

func TestKeyField(t *testing.T) {
	if got := (&Source{}).KeyField(); got != "NodeInfo.Uid" {
		t.Fatalf("KeyField = %q", got)
	}
}

func TestOSFamily(t *testing.T) {
	for in, want := range map[[2]string]string{
		{"", ""}:                     "",
		{"ubuntu 22.04", ""}:         "linux",
		{"rocky 9", "Rocky Linux 9"}: "linux",
		{"Windows 2022", ""}:         "windows",
		{"windows 2019", ""}:         "windows",
		// osType is empty when no CB-Tumblebug extraction pattern matched.
		{"", "Windows Server 2022 Datacenter"}: "windows",
		{"", "Ubuntu 22.04"}:                   "linux",
		{"ubuntu 22.04", "Microsoft Windows"}:  "windows",
	} {
		if got := osFamily(in[0], in[1]); got != want {
			t.Errorf("osFamily(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

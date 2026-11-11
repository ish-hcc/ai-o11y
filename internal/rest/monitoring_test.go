package rest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cloud-barista/ai-o11y/internal/config"
	"github.com/cloud-barista/ai-o11y/internal/model"
	"github.com/cloud-barista/ai-o11y/internal/monitoring"
	"github.com/cloud-barista/ai-o11y/internal/monitoring/backend/promapi"
	"github.com/cloud-barista/ai-o11y/internal/monitoring/catalog"
	"github.com/cloud-barista/ai-o11y/internal/monitoring/collector"
	"github.com/cloud-barista/ai-o11y/internal/monitoring/mapping"
	"github.com/cloud-barista/ai-o11y/internal/monitoring/query"
)

type memStore struct{ targets map[string]monitoring.Target }

func (m *memStore) Save(_ context.Context, t monitoring.Target) error {
	m.targets[t.EntityID] = t
	return nil
}

func (m *memStore) Delete(_ context.Context, key string) error {
	delete(m.targets, key)
	return nil
}

func (m *memStore) Select(_ context.Context, sel monitoring.Selector) ([]monitoring.Target, error) {
	var out []monitoring.Target
	for _, t := range m.targets {
		if sel.Matches(t) {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b monitoring.Target) int { return strings.Compare(a.EntityID, b.EntityID) })
	return out, nil
}

func (m *memStore) Lookup(_ context.Context, key string) (monitoring.Target, error) {
	t, ok := m.targets[key]
	if !ok {
		return t, fmt.Errorf("target %q: %w", key, monitoring.ErrTargetNotFound)
	}
	return t, nil
}

type fakeSource struct{}

func (fakeSource) KeyField() string { return "NodeInfo.Uid" }
func (fakeSource) List(context.Context, monitoring.Scope) ([]monitoring.Target, error) {
	return nil, nil
}
func (fakeSource) Exists(context.Context, monitoring.Target) (bool, error) { return true, nil }
func (fakeSource) Fetch(_ context.Context, ref map[string]string) (monitoring.Target, error) {
	if ref["nodeId"] != "node01" {
		return monitoring.Target{}, monitoring.ErrTargetNotFound
	}
	return monitoring.Target{EntityID: "uid-fetched", Kind: "vm", Name: "web-1", OS: "linux",
		Extra: map[string]string{"nsId": ref["nsId"], "infraId": ref["infraId"], "nodeId": ref["nodeId"]}}, nil
}

type fakeSyncer struct{ err error }

func (f fakeSyncer) Sync(context.Context) (mapping.SyncResult, error) {
	return mapping.SyncResult{Updated: []string{"k1"}, Kept: map[string]string{}}, f.err
}

type fakeBackend struct {
	got    monitoring.QuerySpec
	err    error
	series []monitoring.Series
}

func (f *fakeBackend) WriteEndpoints() []monitoring.Endpoint {
	return []monitoring.Endpoint{{URL: "http://prometheus:9090/api/v1/otlp", Protocol: "otlp_http"}}
}
func (f *fakeBackend) Query(_ context.Context, q monitoring.QuerySpec) ([]monitoring.Series, error) {
	f.got = q
	return f.series, f.err
}
func (f *fakeBackend) Series(context.Context, string) ([]map[string]string, error) { return nil, nil }
func (f *fakeBackend) Capabilities() monitoring.BackendCaps                        { return nil }

func newTestServer(t *testing.T, deps Deps) *Server {
	t.Helper()
	cat, err := catalog.New(catalog.Builtin())
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	deps.Catalog = cat
	deps.Collectors = []monitoring.CollectorKind{collector.Hostmetrics{}, collector.NvidiaGPU{}, collector.AMDGPU{}}
	return NewServer(&config.Config{Server: config.ServerConfig{Port: 8094}}, deps)
}

func call(t *testing.T, s *Server, method, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, BasePath+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.echo.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func fleet() *memStore {
	return &memStore{targets: map[string]monitoring.Target{
		"k1": {EntityID: "k1", Kind: "vm", Name: "web-1", OS: "linux", Extra: map[string]string{"nsId": "ns01", "infraId": "i1", "nodeId": "n1"}},
		"k2": {EntityID: "k2", Kind: "vm", Name: "web-2", OS: "linux", Extra: map[string]string{"nsId": "ns02", "infraId": "i2", "nodeId": "n2"}},
	}}
}

func TestPostTarget(t *testing.T) {
	store := &memStore{targets: map[string]monitoring.Target{}}
	s := newTestServer(t, Deps{Targets: store, Source: fakeSource{}})

	code, body := call(t, s, http.MethodPost, "/targets",
		`{"kind":"node","nsId":"ns01","infraId":"infra01","nodeId":"vm1","uid":"uid-given","name":"web-9","os":"linux","gpuCount":2,"gpuModel":"A100"}`)
	if code != http.StatusOK {
		t.Fatalf("register with description = %d %s", code, body)
	}
	got := store.targets["uid-given"]
	if got.Name != "web-9" || got.GPUCount != 2 || got.Extra["nsId"] != "ns01" || got.Extra["nodeId"] != "vm1" {
		t.Errorf("stored %+v", got)
	}

	if code, body = call(t, s, http.MethodPost, "/targets", `{"kind":"node","nsId":"ns01","infraId":"infra01","nodeId":"node01"}`); code != http.StatusOK {
		t.Fatalf("register by platform names = %d %s", code, body)
	}
	if _, ok := store.targets["uid-fetched"]; !ok {
		t.Error("target read from the platform was not stored")
	}

	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"node not in platform": {`{"kind":"node","nsId":"ns01","infraId":"infra01","nodeId":"gone"}`, http.StatusNotFound},
		"kubernetes":           {`{"kind":"k8s","nsId":"ns01"}`, http.StatusUnprocessableEntity},
		"missing nodeId":       {`{"kind":"node","nsId":"ns01","infraId":"infra01"}`, http.StatusBadRequest},
		"bad os":               {`{"kind":"node","nsId":"a","infraId":"b","nodeId":"c","uid":"u","os":"mac"}`, http.StatusBadRequest},
		"bad json":             {`{`, http.StatusBadRequest},
		"no kind":              {`{"nsId":"a","infraId":"b","nodeId":"c","uid":"u"}`, http.StatusBadRequest},
	} {
		if code, body := call(t, s, http.MethodPost, "/targets", tc.body); code != tc.want {
			t.Errorf("%s: %d %s, want %d", name, code, body, tc.want)
		}
	}

	noSource := newTestServer(t, Deps{Targets: store})
	if code, _ := call(t, noSource, http.MethodPost, "/targets", `{"kind":"node","nsId":"a","infraId":"b","nodeId":"c"}`); code != http.StatusServiceUnavailable {
		t.Errorf("register by names without a platform = %d, want 503", code)
	}
}

func TestTargetReads(t *testing.T) {
	s := newTestServer(t, Deps{Targets: fleet(), Backend: &fakeBackend{}})

	code, body := call(t, s, http.MethodGet, "/targets?nsId=ns01", "")
	var list model.TargetListResp
	if code != http.StatusOK || json.Unmarshal([]byte(body), &list) != nil || len(list.Targets) != 1 || list.Targets[0].ID != "k1" {
		t.Errorf("list by nsId = %d %s", code, body)
	}
	if code, body = call(t, s, http.MethodGet, "/targets", ""); code != http.StatusOK || !strings.Contains(body, `"k2"`) {
		t.Errorf("list all = %d %s", code, body)
	}
	if code, _ = call(t, s, http.MethodGet, "/targets/k2", ""); code != http.StatusOK {
		t.Errorf("get = %d", code)
	}
	if code, _ = call(t, s, http.MethodGet, "/targets/nope", ""); code != http.StatusNotFound {
		t.Errorf("get unknown = %d", code)
	}
	code, body = call(t, s, http.MethodGet, "/targets/k1/collectorConfig", "")
	if code != http.StatusOK || !strings.Contains(body, "entity_id") || !strings.Contains(body, "http://prometheus:9090/api/v1/otlp") {
		t.Errorf("collector config = %d %s", code, body)
	}
	if code, _ = call(t, s, http.MethodDelete, "/targets/k1", ""); code != http.StatusOK {
		t.Errorf("delete = %d", code)
	}
	if code, _ = call(t, s, http.MethodDelete, "/targets/k1", ""); code != http.StatusNotFound {
		t.Errorf("delete again = %d", code)
	}

	none := newTestServer(t, Deps{})
	for _, path := range []string{"/targets", "/targets/k1", "/targets/k1/collectorConfig"} {
		if code, _ := call(t, none, http.MethodGet, path, ""); code != http.StatusServiceUnavailable {
			t.Errorf("GET %s without a store = %d, want 503", path, code)
		}
	}
}

func TestSync(t *testing.T) {
	ok := newTestServer(t, Deps{Targets: fleet(), Syncer: fakeSyncer{}})
	if code, body := call(t, ok, http.MethodPost, "/targets/sync", ""); code != http.StatusOK ||
		body != `{"updated":["k1"],"deleted":[],"kept":{}}`+"\n" {
		t.Errorf("sync = %d %s", code, body)
	}
	for err, want := range map[error]int{
		mapping.ErrSyncRunning:                        http.StatusConflict,
		fmt.Errorf("%w: boom", mapping.ErrListFailed): http.StatusBadGateway,
		errors.New("db down"):                         http.StatusInternalServerError,
	} {
		s := newTestServer(t, Deps{Targets: fleet(), Syncer: fakeSyncer{err: err}})
		if code, _ := call(t, s, http.MethodPost, "/targets/sync", ""); code != want {
			t.Errorf("sync with %v = %d, want %d", err, code, want)
		}
	}
	if code, _ := call(t, newTestServer(t, Deps{Targets: fleet()}), http.MethodPost, "/targets/sync", ""); code != http.StatusServiceUnavailable {
		t.Errorf("sync without a platform = %d, want 503", code)
	}
}

func TestMetrics(t *testing.T) {
	code, body := call(t, newTestServer(t, Deps{}), http.MethodGet, "/metrics", "")
	var list model.MetricListResp
	if code != http.StatusOK || json.Unmarshal([]byte(body), &list) != nil {
		t.Fatalf("metrics = %d", code)
	}
	byName := map[string]model.MetricInfo{}
	for _, m := range list.Metrics {
		byName[m.Name] = m
	}
	if m := byName["system.cpu.utilization"]; !m.Supported || m.Schema != "" || m.Attribute == nil || m.Attribute.Name != "cpu.mode" {
		t.Errorf("cpu utilization = %+v, want a supported common field split by cpu.mode", m)
	}
	if m := byName["system.cpu.load_average.1m"]; m.Schema != "hostmetrics" {
		t.Errorf("load average = %+v, want hostmetrics' own field", m)
	}
	if m := byName["system.paging.operations"]; m.Supported || m.Gap == "" {
		t.Errorf("paging operations = %+v, want unsupported with a reason", m)
	}
	if m := byName["system.process.count"]; !m.ZeroFill {
		t.Errorf("process count = %+v, want zeroFill", m)
	}
	if m := byName["system.disk.io"]; m.Dimension != "system.device.name" {
		t.Errorf("disk io dimension = %q, want the OpenTelemetry name", m.Dimension)
	}
}

func TestTargetMetrics(t *testing.T) {
	store := fleet()
	windows := store.targets["k2"]
	windows.OS = "windows"
	store.targets["k2"] = windows
	unknown := monitoring.Target{EntityID: "k3", Kind: "vm"}
	store.targets["k3"] = unknown
	s := newTestServer(t, Deps{Targets: store})

	status := func(id string) map[string]model.TargetMetricInfo {
		t.Helper()
		code, body := call(t, s, http.MethodGet, "/targets/"+id+"/metrics", "")
		var resp model.TargetMetricListResp
		if code != http.StatusOK || json.Unmarshal([]byte(body), &resp) != nil {
			t.Fatalf("target metrics %s = %d %s", id, code, body)
		}
		out := map[string]model.TargetMetricInfo{}
		for _, m := range resp.Metrics {
			out[m.Name] = m
		}
		return out
	}
	linux := status("k1")
	if !linux["system.cpu.utilization"].Supported || !linux["system.cpu.load_average.1m"].Supported {
		t.Errorf("linux vm: %+v / %+v", linux["system.cpu.utilization"], linux["system.cpu.load_average.1m"])
	}
	if m := linux["system.paging.operations"]; m.Supported || m.Reason == "" {
		t.Errorf("paging operations on linux = %+v, want unsupported with the gap", m)
	}
	win := status("k2")
	if m := win["system.cpu.load_average.1m"]; m.Supported || m.Reason != "Needs linux" {
		t.Errorf("load average on windows = %+v", m)
	}
	if !win["system.cpu.utilization"].Supported {
		t.Error("cpu utilization on windows is unsupported")
	}
	if m := status("k3")["system.process.count"]; m.Supported || !strings.Contains(m.Reason, "unknown") {
		t.Errorf("process count with unknown OS = %+v", m)
	}
	gpuUnknown := monitoring.Target{EntityID: "k4", Kind: "vm", OS: "linux", GPUCount: 1}
	store.targets["k4"] = gpuUnknown
	if m := status("k4")["hw.gpu.utilization"]; m.Supported || !strings.Contains(m.Reason, "NVIDIA") {
		t.Errorf("gpu with no model = %+v, want unsupported naming the vendor", m)
	}
	gpu := gpuUnknown
	gpu.EntityID, gpu.GPUModel = "k5", "NVIDIA T4"
	store.targets["k5"] = gpu
	if m := status("k5")["hw.gpu.utilization"]; !m.Supported {
		t.Errorf("nvidia gpu = %+v", m)
	}
	// CB-Tumblebug names the model of Azure Standard_NV4ads_V710_v5 but not
	// the count: the model alone makes the AMD collector run.
	amd := monitoring.Target{EntityID: "k6", Kind: "vm", OS: "linux", GPUModel: "AMD RADEON PRO V710"}
	store.targets["k6"] = amd
	amdStatus := status("k6")
	if m := amdStatus["temperature.edge"]; !m.Supported {
		t.Errorf("amd gpu with no count, amd-smi field = %+v, want supported", m)
	}
	if m := amdStatus["hw.gpu.utilization"]; m.Supported || m.Reason != "The collector for the target's GPU does not provide it" {
		t.Errorf("amd gpu with no count, nvidia-only field = %+v", m)
	}
	if m := status("k1")["hw.gpu.utilization"]; m.Supported || m.Reason != "No collector running on this target provides it" {
		t.Errorf("gpu field on a vm without a gpu = %+v", m)
	}
	if code, _ := call(t, s, http.MethodGet, "/targets/nope/metrics", ""); code != http.StatusNotFound {
		t.Errorf("unknown target = %d", code)
	}
}

func TestQuery(t *testing.T) {
	at := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	backend := &fakeBackend{series: []monitoring.Series{
		{Labels: map[string]string{"entity_id": "k1"}, Points: []monitoring.Point{{T: at, V: 0.5}, {T: at.Add(time.Minute), V: math.NaN()}}},
	}}
	s := newTestServer(t, Deps{Targets: fleet(), Backend: backend})

	code, body := call(t, s, http.MethodPost, "/query", `{"metric":"system.cpu.utilization","selector":{"nsId":"ns01"},
		"step":"1m","start":"2026-11-06T00:00:00Z","end":"2026-11-06T01:00:00Z","windowFn":"max","windowRange":"5m",
		"topK":{"n":3,"desc":true},"fillZero":true}`)
	if code != http.StatusOK {
		t.Fatalf("query = %d %s", code, body)
	}
	q := backend.got
	if len(q.Targets) != 1 || q.Targets[0].EntityID != "k1" || q.Step != time.Minute || q.Window.Range != 5*time.Minute ||
		q.Window.Fn != monitoring.AggMax || q.TopK == nil || q.TopK.N != 3 || !q.FillZero {
		t.Errorf("spec sent = %+v", q)
	}
	want := `{"series":[{"labels":{"entity_id":"k1"},"points":[{"t":"2026-11-06T00:00:00Z","v":0.5},{"t":"2026-11-06T00:01:00Z","v":null}]}],` +
		`"targets":{"k1":{"id":"k1","kind":"vm","name":"web-1","provider":"","region":"","os":"linux","gpuCount":0,"gpuModel":"","attributes":{"infraId":"i1","nodeId":"n1","nsId":"ns01"}}}}` + "\n"
	if body != want {
		t.Errorf("body =\n%s\nwant\n%s", body, want)
	}

	if code, _ = call(t, s, http.MethodPost, "/query", `{"metric":"system.cpu.utilization","targetIds":["k2"]}`); code != http.StatusOK ||
		backend.got.Targets[0].EntityID != "k2" {
		t.Errorf("query by id = %d", code)
	}
	backend.series = []monitoring.Series{{Labels: map[string]string{"entity_id": "k1", "cpu.mode": "user"},
		Points: []monitoring.Point{{T: at, V: 0.25}}}}
	code, body = call(t, s, http.MethodPost, "/query", `{"metric":"system.cpu.utilization","targetIds":["k1"],"splitBy":["cpu.mode"],"where":{"cpu.mode":"user"}}`)
	if code != http.StatusOK || !strings.Contains(body, `"cpu.mode":"user"`) || strings.Contains(body, "cpu_mode") {
		t.Errorf("split query = %d %s, want the attribute by its OpenTelemetry name", code, body)
	}
	if q := backend.got; q.Where["cpu.mode"] != "user" || len(q.SplitBy) != 1 {
		t.Errorf("split spec sent = %+v", q)
	}
	code, _ = call(t, s, http.MethodPost, "/query", `{"metric":"system.cpu.load_average.1m","schema":"hostmetrics","targetIds":["k1"]}`)
	if code != http.StatusOK || backend.got.Metric != "hostmetrics:system.cpu.load_average.1m" {
		t.Errorf("own field query = %d, metric sent %q", code, backend.got.Metric)
	}
	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"unknown metric":           {`{"metric":"nope","targetIds":["k1"]}`, http.StatusNotFound},
		"unsupported":              {`{"metric":"system.paging.operations","targetIds":["k1"]}`, http.StatusUnprocessableEntity},
		"unknown target":           {`{"metric":"system.cpu.utilization","targetIds":["zz"]}`, http.StatusNotFound},
		"no targets named":         {`{"metric":"system.cpu.utilization"}`, http.StatusBadRequest},
		"bad step":                 {`{"metric":"system.cpu.utilization","targetIds":["k1"],"step":"x"}`, http.StatusBadRequest},
		"graph no range":           {`{"metric":"system.cpu.utilization","targetIds":["k1"],"step":"1m"}`, http.StatusBadRequest},
		"own field without schema": {`{"metric":"system.cpu.load_average.1m","targetIds":["k1"]}`, http.StatusNotFound},
		"other attribute":          {`{"metric":"system.cpu.utilization","targetIds":["k1"],"splitBy":["system.memory.state"]}`, http.StatusBadRequest},
		"no attribute":             {`{"metric":"system.uptime","targetIds":["k1"],"where":{"cpu.mode":"user"}}`, http.StatusBadRequest},
		"unknown value":            {`{"metric":"system.cpu.utilization","targetIds":["k1"],"where":{"cpu.mode":"$sel"}}`, http.StatusBadRequest},
	} {
		if code, body := call(t, s, http.MethodPost, "/query", tc.body); code != tc.want {
			t.Errorf("%s: %d %s, want %d", name, code, body, tc.want)
		}
	}

	backend.err = &promapi.APIError{StatusCode: 422, Type: "execution", Message: "store down"}
	if code, _ := call(t, s, http.MethodPost, "/query", `{"metric":"system.cpu.utilization","targetIds":["k1"]}`); code != http.StatusBadGateway {
		t.Errorf("backend failure = %d, want 502", code)
	}
	backend.err = fmt.Errorf("%w: window needs both a function and a positive range", query.ErrInvalidQuery)
	code, body = call(t, s, http.MethodPost, "/query", `{"metric":"system.cpu.utilization","targetIds":["k1"]}`)
	if code != http.StatusBadRequest || strings.Contains(body, "window needs") {
		t.Errorf("unbuildable query = %d %s, want 400 without the internal reason", code, body)
	}
	backend.err = &promapi.APIError{StatusCode: 400, Type: "bad_data", Message: "exceeded maximum resolution of 11,000 points"}
	if code, _ := call(t, s, http.MethodPost, "/query", `{"metric":"system.cpu.utilization","targetIds":["k1"]}`); code != http.StatusBadRequest {
		t.Errorf("backend refusing the query = %d, want 400", code)
	}
	backend.err = errors.New("decode failed")
	if code, _ := call(t, s, http.MethodPost, "/query", `{"metric":"system.cpu.utilization","targetIds":["k1"]}`); code != http.StatusBadGateway {
		t.Errorf("other backend failure = %d, want 502", code)
	}
}

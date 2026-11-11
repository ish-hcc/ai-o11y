package collector

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloud-barista/ai-o11y/internal/monitoring"
	"github.com/cloud-barista/ai-o11y/internal/monitoring/catalog"
)

var update = flag.Bool("update", false, "rewrite golden files")

func builtin(t *testing.T) *catalog.Catalog {
	t.Helper()
	c, err := catalog.New(catalog.Builtin())
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	return c
}

func vm() monitoring.Target {
	return monitoring.Target{
		EntityID: "d44ikdomt0tac4ejrmmg",
		Kind:     "vm",
		Name:     "web-1",
		Provider: "aws",
		Region:   "ap-northeast-2",
		OS:       "linux",
		Extra:    map[string]string{"nsId": "ns01"},
	}
}

var (
	kinds    = []monitoring.CollectorKind{Hostmetrics{}, NvidiaGPU{}, AMDGPU{}}
	endpoint = []monitoring.Endpoint{{URL: "http://prometheus:9090/api/v1/otlp"}}
)

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	golden := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("config differs from %s:\n%s", golden, got)
	}
}

// TestRenderSnapshot pins the whole collector configuration for a VM. Run with
// -update to rewrite after a deliberate change.
func TestRenderSnapshot(t *testing.T) {
	got, err := Render(vm(), kinds, builtin(t), endpoint, Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	checkGolden(t, "vm.golden.yaml", got)
}

func TestRenderIsDeterministic(t *testing.T) {
	c := builtin(t)
	target := vm()
	first, err := Render(target, kinds, c, endpoint, Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for range 20 {
		next, err := Render(target, kinds, c, endpoint, Options{})
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		if string(next) != string(first) {
			t.Fatal("Render output changed between calls")
		}
	}
}

func TestRenderProcessorOrder(t *testing.T) {
	extra := fixedKind{frag: monitoring.ConfigFragment{
		Receivers:  map[string]any{"otlp": map[string]any{}},
		Processors: map[string]any{"zzz": map[string]any{}},
	}}
	got, err := Render(vm(), []monitoring.CollectorKind{extra}, builtin(t), endpoint, Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := "processors:\n                - zzz\n                - " + monitoring.IdentityProcessor + "\n                - " + BatchProcessor
	if !strings.Contains(string(got), want) {
		t.Fatalf("processors are not ordered other, identity, batch:\n%s", got)
	}
}

func TestRenderRejects(t *testing.T) {
	c := builtin(t)
	unmapped := vm()
	unmapped.EntityID = ""
	cases := map[string]struct {
		target   monitoring.Target
		kinds    []monitoring.CollectorKind
		endpoint []monitoring.Endpoint
	}{
		"target without key": {unmapped, kinds, endpoint},
		"no endpoint":        {vm(), kinds, nil},
		"endpoint no URL":    {vm(), kinds, []monitoring.Endpoint{endpoint[0], {}}},
		"no kind applies":    {monitoring.Target{Kind: "datastore", EntityID: "x"}, kinds, endpoint},
		"duplicate id":       {vm(), []monitoring.CollectorKind{Hostmetrics{}, Hostmetrics{}}, endpoint},
		"batch clashes":      {vm(), []monitoring.CollectorKind{Hostmetrics{}, fixedKind{frag: monitoring.ConfigFragment{Processors: map[string]any{BatchProcessor: nil}}}}, endpoint},
		"identity clashes":   {vm(), []monitoring.CollectorKind{Hostmetrics{}, fixedKind{frag: monitoring.ConfigFragment{Processors: map[string]any{monitoring.IdentityProcessor: nil}}}}, endpoint},
	}
	for name, tc := range cases {
		if _, err := Render(tc.target, tc.kinds, c, tc.endpoint, Options{}); err == nil {
			t.Errorf("%s: Render succeeded", name)
		}
	}
}

func TestHostmetricsRejectsUnknownMetric(t *testing.T) {
	rows := []monitoring.MetricRow{{
		Name: "host.mystery", Area: "cpu", Unit: "1", Kind: monitoring.KindGauge,
		Exprs: map[string]string{catalog.CollectorHostmetrics: `sum by (entity_id) (system_mystery_total{$sel})`},
	}}
	c, err := catalog.New(rows)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if _, err := (Hostmetrics{}).Render(vm(), c); err == nil {
		t.Fatal("Render accepted a metric no scraper emits")
	}
}

func TestScraperOf(t *testing.T) {
	for name, want := range map[string]string{
		"system_cpu_load_average_1m_ratio": "load",
		"system.cpu.load_average.15m":      "load",
		"system_cpu_time_seconds_total":    "cpu",
		"system.cpu.physical.count":        "cpu",
		"system.linux.memory.available":    "memory",
		"system_memory_usage_bytes":        "memory",
		"system_paging_operations_total":   "paging",
		"system_filesystem_usage_bytes":    "filesystem",
		"system_disk_io_bytes_total":       "disk",
		"system_network_io_bytes_total":    "network",
		"system_processes_count":           "processes",
		"system_uptime_seconds":            "system",
		"process.cpu.utilization":          "process",
	} {
		got, err := scraperOf(name)
		if err != nil || got != want {
			t.Errorf("scraperOf(%q) = %q, %v; want %q", name, got, err, want)
		}
	}
}

type fixedKind struct{ frag monitoring.ConfigFragment }

func (fixedKind) Name() string                     { return "fixed" }
func (fixedKind) Path() monitoring.Path            { return monitoring.PathAgent }
func (fixedKind) AppliesTo(monitoring.Target) bool { return true }
func (k fixedKind) Render(monitoring.Target, monitoring.MetricCatalog) (monitoring.ConfigFragment, error) {
	return k.frag, nil
}
func (fixedKind) Health(context.Context, monitoring.Target) (monitoring.CollectorHealth, error) {
	return monitoring.CollectorHealth{}, nil
}

func gpuVM() monitoring.Target {
	t := vm()
	t.GPUCount, t.GPUModel = 2, "NVIDIA A100-SXM4-80GB"
	return t
}

// TestRenderGPUSnapshot is a VM with GPUs: the NVIDIA GPU receiver joins
// hostmetrics in the one metrics pipeline.
func TestRenderGPUSnapshot(t *testing.T) {
	got, err := Render(gpuVM(), kinds, builtin(t), endpoint, Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	checkGolden(t, "gpu-vm.golden.yaml", got)
}

func TestNvidiaGPUAppliesTo(t *testing.T) {
	for name, tc := range map[string]struct {
		count int
		model string
		want  bool
	}{
		"no gpu":        {0, "", false},
		"nvidia":        {1, "NVIDIA T4", true},
		"model unknown": {1, "", false},
		"tumblebug id":  {1, "nvidia-a100-80gb", true},
		"amd":           {1, "AMD Instinct MI300X", false},
		"intel":         {1, "Intel Gaudi 2", false},
	} {
		target := vm()
		target.GPUCount, target.GPUModel = tc.count, tc.model
		if got := (NvidiaGPU{}).AppliesTo(target); got != tc.want {
			t.Errorf("%s: AppliesTo = %v, want %v", name, got, tc.want)
		}
	}
}

func amdGPUVM() monitoring.Target {
	t := vm()
	t.GPUCount, t.GPUModel = 1, "AMD RADEON PRO V710"
	return t
}

// TestRenderAMDGPUSnapshot is a VM with an AMD GPU: the AMD GPU receiver,
// not the NVIDIA one, joins hostmetrics.
func TestRenderAMDGPUSnapshot(t *testing.T) {
	got, err := Render(amdGPUVM(), kinds, builtin(t), endpoint, Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	checkGolden(t, "amd-gpu-vm.golden.yaml", got)
}

func TestAMDGPUAppliesTo(t *testing.T) {
	for name, tc := range map[string]struct {
		kind  string
		count int
		model string
		want  bool
	}{
		"no gpu":        {"vm", 0, "AMD RADEON PRO V710", false},
		"radeon":        {"vm", 1, "AMD RADEON PRO V710", true},
		"instinct":      {"vm", 1, "AMD INSTINCT", true},
		"mi300x":        {"vm", 8, "AMD MI300X", true},
		"no vendor":     {"vm", 1, "Radeon Pro V520", true},
		"product only":  {"vm", 1, "Instinct MI250", true},
		"nvidia":        {"vm", 1, "NVIDIA T4", false},
		"model unknown": {"vm", 1, "", false},
		"not a vm":      {"k8s", 1, "AMD MI300X", false},
	} {
		target := vm()
		target.Kind, target.GPUCount, target.GPUModel = tc.kind, tc.count, tc.model
		if got := (AMDGPU{}).AppliesTo(target); got != tc.want {
			t.Errorf("%s: AppliesTo = %v, want %v", name, got, tc.want)
		}
	}
}

// TestRenderTelemetry: the collector's own metrics are off by default, so it
// opens no port; a port turns them on, on localhost.
func TestRenderTelemetry(t *testing.T) {
	off, err := Render(vm(), kinds, builtin(t), endpoint, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(off), "level: none") || strings.Contains(string(off), "port:") {
		t.Errorf("default telemetry:\n%s", off)
	}
	on, err := Render(vm(), kinds, builtin(t), endpoint, Options{TelemetryPort: 18889})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(on), "port: 18889") || !strings.Contains(string(on), "host: 127.0.0.1") {
		t.Errorf("telemetry on a port:\n%s", on)
	}
}

// failover is two write endpoints, as two vmauths in front of replicated
// storage.
var failover = []monitoring.Endpoint{
	{URL: "http://vmauth-1:8427/opentelemetry"},
	{URL: "http://vmauth-2:8427/opentelemetry"},
}

// TestRenderFailover pins the configuration for several write endpoints: an
// exporter per endpoint with its queue and retries off, a pipeline per
// priority, and the failover connector between them and the metrics pipeline.
func TestRenderFailover(t *testing.T) {
	got, err := Render(vm(), kinds, builtin(t), failover, Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	checkGolden(t, "vm-failover.golden.yaml", got)
}

// TestRenderWriteAccount pins the configuration for write endpoints with an
// account: a basic auth extension per exporter whose password is an
// environment reference, never the password itself.
func TestRenderWriteAccount(t *testing.T) {
	withAuth := make([]monitoring.Endpoint, len(failover))
	for i, e := range failover {
		e.BasicAuth = &monitoring.BasicAuth{Username: "writer", PasswordEnv: WritePasswordEnv}
		withAuth[i] = e
	}
	got, err := Render(vm(), kinds, builtin(t), withAuth, Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	checkGolden(t, "vm-failover-auth.golden.yaml", got)
	if !strings.Contains(string(got), "${env:"+WritePasswordEnv+"}") {
		t.Error("the password is not taken from the environment")
	}
	single, err := Render(vm(), kinds, builtin(t), withAuth[:1], Options{})
	if err != nil || !strings.Contains(string(single), "authenticator: "+ExtensionBasicAuth+"\n") {
		t.Errorf("single endpoint with an account = %v\n%s", err, single)
	}
	bad := []monitoring.Endpoint{{URL: "http://vmauth:8427/opentelemetry", BasicAuth: &monitoring.BasicAuth{Username: "writer"}}}
	if _, err := Render(vm(), kinds, builtin(t), bad, Options{}); err == nil {
		t.Error("an account without a password variable rendered")
	}
}

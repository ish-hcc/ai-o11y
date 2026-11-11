package nvidiagpureceiver

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fakeSMI answers --help-query-gpu and --query-gpu from recorded output.
func fakeSMI(t *testing.T, help, query string) (*nvidiaSMI, *[]string) {
	t.Helper()
	var asked []string
	s := newNvidiaSMI(NvidiaSMIConfig{Path: "nvidia-smi", Fields: DefaultFields})
	s.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		asked = append(asked, strings.Join(args, " "))
		if args[0] == "--help-query-gpu" {
			return read(t, help), nil
		}
		return read(t, query), nil
	}
	return s, &asked
}

// TestReadRecordedGTX1050Ti reads output recorded from a GeForce GTX 1050 Ti
// on driver 580.178.04: the fields it does not support are [N/A] or N/A and
// are left out.
func TestReadRecordedGTX1050Ti(t *testing.T) {
	s, asked := fakeSMI(t, "help-query-gpu-580.txt", "query-gtx1050ti-580.csv")
	gpus, err := s.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(gpus) != 1 {
		t.Fatalf("gpus = %d", len(gpus))
	}
	g := gpus[0]
	if g.Attributes["index"] != "0" || g.Attributes["name"] != "NVIDIA GeForce GTX 1050 Ti" || !strings.HasPrefix(g.Attributes["uuid"], "GPU-") ||
		len(g.Attributes) != 3 {
		t.Errorf("attributes = %v", g.Attributes)
	}
	if g.PCIBusID != "00000000:01:00.0" {
		t.Errorf("PCI bus id = %q", g.PCIBusID)
	}
	got := map[string]Value{}
	for _, v := range g.Values {
		got[v.Field] = v
	}
	for field, want := range map[string]Value{
		"utilization.gpu":             {Unit: "%", Value: 0},
		"memory.total":                {Unit: "MiBy", Value: 4096},
		"memory.used":                 {Unit: "MiBy", Value: 1315},
		"temperature.gpu":             {Unit: "Cel", Value: 33},
		"fan.speed":                   {Unit: "%", Value: 40},
		"clocks.sm":                   {Unit: "MHz", Value: 139},
		"pstate":                      {Value: 0},
		"clocks_event_reasons.active": {Value: 1},
	} {
		if v, ok := got[field]; !ok || v.Unit != want.Unit || v.Value != want.Value {
			t.Errorf("%s = %+v (present %v), want %+v", field, v, ok, want)
		}
	}
	for _, field := range []string{"power.draw", "temperature.memory", "ecc.errors.corrected.volatile.total", "remapped_rows.pending"} {
		if _, ok := got[field]; ok {
			t.Errorf("%s is not supported on this GPU but was read", field)
		}
	}
	if len(*asked) != 2 || !strings.HasPrefix((*asked)[1], "--query-gpu=index,uuid,name,pci.bus_id,utilization.gpu,") {
		t.Errorf("asked %q", *asked)
	}
	if _, err := s.Read(context.Background()); err != nil || len(*asked) != 3 {
		t.Errorf("second read asked %q, want the field list kept", *asked)
	}
}

func TestReadTwoGPUs(t *testing.T) {
	fields := []string{"index", "uuid", "name", "utilization.gpu", "memory.used", "temperature.gpu", "power.draw",
		"pstate", "ecc.errors.uncorrected.volatile.total", "remapped_rows.pending", "retired_pages.pending"}
	gpus, err := parseQuery(read(t, "query-2gpu-datacenter.csv"), fields)
	if err != nil {
		t.Fatalf("parseQuery: %v", err)
	}
	if len(gpus) != 2 {
		t.Fatalf("gpus = %d", len(gpus))
	}
	second := map[string]float64{}
	for _, v := range gpus[1].Values {
		second[v.Field] = v.Value
	}
	if _, ok := second["utilization.gpu"]; ok {
		t.Error("[Not Supported] was read as a value")
	}
	if second["pstate"] != 8 || second["power.draw"] != 61.2 || second["ecc.errors.uncorrected.volatile.total"] != 2 ||
		second["remapped_rows.pending"] != 1 || second["retired_pages.pending"] != 0 {
		t.Errorf("second GPU = %v", second)
	}
}

func TestSupportedFieldsDropsUnknown(t *testing.T) {
	s, _ := fakeSMI(t, "help-query-gpu-580.txt", "query-gtx1050ti-580.csv")
	s.fields = []string{"utilization.gpu", "no.such.field", "clocks.sm", "utilization.gpu"}
	got, err := s.supportedFields(context.Background())
	if err != nil || !slices.Equal(got, []string{"utilization.gpu", "clocks.sm"}) {
		t.Errorf("supported = %v, %v", got, err)
	}
}

func TestReadRetriesHelpAfterFailure(t *testing.T) {
	s := newNvidiaSMI(NvidiaSMIConfig{Path: "nvidia-smi", Fields: []string{"utilization.gpu"}})
	calls := 0
	s.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("driver not loaded")
		}
		if args[0] == "--help-query-gpu" {
			return []byte(`"utilization.gpu"` + "\n"), nil
		}
		return []byte("index, uuid, name, pci.bus_id, utilization.gpu [%]\n0, GPU-x, T4, 00000001:00:00.0, 5\n"), nil
	}
	if _, err := s.Read(context.Background()); err == nil {
		t.Fatal("Read succeeded while nvidia-smi failed")
	}
	gpus, err := s.Read(context.Background())
	if err != nil || len(gpus) != 1 || gpus[0].Values[0].Value != 5 {
		t.Errorf("Read after recovery = %+v, %v", gpus, err)
	}
}

func TestParseQueryRejectsShapeMismatch(t *testing.T) {
	if _, err := parseQuery([]byte("index, uuid\n0, a\n"), []string{"index", "uuid", "name"}); err == nil {
		t.Error("header with fewer columns than fields was accepted")
	}
	if _, err := parseQuery([]byte(""), []string{"index"}); err == nil {
		t.Error("empty answer was accepted")
	}
}

// TestRealNvidiaSMI runs the installed nvidia-smi when AIO11Y_NVIDIA_SMI is
// set.
func TestRealNvidiaSMI(t *testing.T) {
	if os.Getenv("AIO11Y_NVIDIA_SMI") == "" {
		t.Skip("AIO11Y_NVIDIA_SMI is not set")
	}
	gpus, err := newNvidiaSMI(NvidiaSMIConfig{Path: "nvidia-smi", Fields: DefaultFields}).Read(context.Background())
	if err != nil || len(gpus) == 0 || len(gpus[0].Values) == 0 {
		t.Fatalf("Read = %+v, %v", gpus, err)
	}
	t.Logf("%d GPUs, %d values on the first: %v", len(gpus), len(gpus[0].Values), gpus[0].Values)
}

// TestRunCommandStopsWaiting: a command that outlives its context is killed
// and the call returns, instead of blocking the scrape.
func TestRunCommandStopsWaiting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := runCommand(ctx, "sleep", "30"); err == nil {
		t.Fatal("a killed command succeeded")
	}
	if d := time.Since(start); d > waitDelay+time.Second {
		t.Errorf("returned after %v", d)
	}
}

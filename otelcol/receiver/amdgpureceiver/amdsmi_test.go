package amdgpureceiver

import (
	"context"
	"errors"
	"os"
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

// fakeSMI answers amd-smi static, list and metric from recorded output.
func fakeSMI(t *testing.T, static, list, metric string) (*amdSMI, *[]string) {
	t.Helper()
	var asked []string
	s := newAMDSMI(AMDSMIConfig{Path: "amd-smi"})
	s.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		asked = append(asked, strings.Join(args, " "))
		switch args[0] {
		case "static":
			return read(t, static), nil
		case "list":
			return read(t, list), nil
		}
		return read(t, metric), nil
	}
	return s, &asked
}

func byField(g GPU) map[string]Value {
	got := map[string]Value{}
	for _, v := range g.Values {
		got[v.Field] = v
	}
	return got
}

func checkValues(t *testing.T, got map[string]Value, want map[string]Value) {
	t.Helper()
	for field, w := range want {
		if v, ok := got[field]; !ok || v.Unit != w.Unit || v.Value != w.Value {
			t.Errorf("%s = %+v (present %v), want %+v", field, v, ok, w)
		}
	}
}

// TestReadV710VirtualFunction reads answers recorded on an Azure V710 VM, a
// virtual function: usage is "N/A" as a whole, power, temperature, clocks and
// PCIe are "N/A", and only memory use and the ECC counts are numbers.
func TestReadV710VirtualFunction(t *testing.T) {
	s, asked := fakeSMI(t, "static-asic-v710-recorded.json", "list-v710-recorded.json", "metric-v710-recorded.json")
	gpus, err := s.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(gpus) != 1 {
		t.Fatalf("gpus = %d", len(gpus))
	}
	g := gpus[0]
	if g.Attributes["index"] != "0" || g.Attributes["uuid"] != "46007461-0000-1000-80c0-38ba93f59d90" ||
		g.Attributes["name"] != "AMD Radeon Pro V710 MxGPU" {
		t.Errorf("attributes = %v", g.Attributes)
	}
	got := byField(g)
	checkValues(t, got, map[string]Value{
		"mem_usage.total_vram":          {Unit: "MiBy", Value: 4448},
		"mem_usage.used_vram":           {Unit: "MiBy", Value: 227},
		"mem_usage.free_vram":           {Unit: "MiBy", Value: 4221},
		"mem_usage.total_gtt":           {Unit: "MiBy", Value: 7994},
		"mem_usage.used_gtt":            {Unit: "MiBy", Value: 10},
		"ecc.total_correctable_count":   {Value: 0},
		"ecc.total_uncorrectable_count": {Value: 0},
		"ecc.total_deferred_count":      {Value: 0},
	})
	for _, field := range []string{"usage.gfx_activity", "usage.umc_activity", "power.socket_power", "temperature.edge",
		"temperature.hotspot", "clock.gfx_0.clk", "clock.mem_0.clk", "pcie.width", "pcie.replay_count"} {
		if _, ok := got[field]; ok {
			t.Errorf("%s is N/A but was read", field)
		}
	}
	if len(got) != 12 {
		t.Errorf("read %d values, want the 9 mem_usage ones and the 3 ECC counts: %v", len(got), got)
	}
	want := []string{"static --asic --json", "list --json", "metric --json"}
	if strings.Join(*asked, "|") != strings.Join(want, "|") {
		t.Errorf("asked %q, want %q", *asked, want)
	}
	if _, err := s.Read(context.Background()); err != nil || len(*asked) != 4 || (*asked)[3] != "metric --json" {
		t.Errorf("second read asked %q, want the identity kept", *asked)
	}
}

// TestReadMI300X reads the newer {"gpu_data": [...]} format with two
// partitions, and a static answer that names no GPU of the list.
func TestReadMI300X(t *testing.T) {
	s, _ := fakeSMI(t, "static-mi300a-doc.json", "list-v710-rocm10.json", "mi300x-dpx-metric.json")
	gpus, err := s.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(gpus) != 2 || gpus[1].Attributes["index"] != "1" {
		t.Fatalf("gpus = %+v", gpus)
	}
	if _, ok := gpus[1].Attributes["uuid"]; ok {
		t.Errorf("gpu 1 has a uuid though list did not name it: %v", gpus[1].Attributes)
	}
	got := byField(gpus[0])
	checkValues(t, got, map[string]Value{
		"usage.gfx_activity":              {Unit: "%", Value: 0},
		"power.socket_power":              {Unit: "W", Value: 139},
		"temperature.hotspot":             {Unit: "Cel", Value: 41},
		"temperature.mem":                 {Unit: "Cel", Value: 36},
		"mem_usage.total_vram":            {Unit: "MiBy", Value: 196592},
		"mem_usage.used_vram":             {Unit: "MiBy", Value: 283},
		"clock.gfx_0.clk":                 {Unit: "MHz", Value: 132},
		"clock.gfx_1.clk":                 {Unit: "MHz", Value: 131},
		"clock.mem_0.clk":                 {Unit: "MHz", Value: 900},
		"clock.socclk_0.clk":              {Unit: "MHz", Value: 28},
		"energy.total_energy_consumption": {Unit: "J", Value: 109243693.399},
		"pcie.width":                      {Value: 16},
		"ecc.total_uncorrectable_count":   {Value: 0},
	})
	if v := byField(gpus[1])["mem_usage.used_vram"]; v.Value != 492 {
		t.Errorf("gpu 1 used_vram = %+v", v)
	}
	for _, field := range []string{"usage.mm_activity", "temperature.edge", "power.gfx_voltage"} {
		if _, ok := got[field]; ok {
			t.Errorf("%s is N/A but was read", field)
		}
	}
	for _, field := range []string{"usage.mm_activity", "temperature.edge", "power.gfx_voltage"} {
		if _, ok := got[field]; ok {
			t.Errorf("%s is N/A but was read", field)
		}
	}
}

// TestReadROCm61BareArray reads the bare array the amd-smi of ROCm 6.1 writes.
func TestReadROCm61BareArray(t *testing.T) {
	s, _ := fakeSMI(t, "static-asic-v710-recorded.json", "list-v710-recorded.json", "metric-power-rocm61.json")
	gpus, err := s.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(gpus) != 1 {
		t.Fatalf("gpus = %d", len(gpus))
	}
	checkValues(t, byField(gpus[0]), map[string]Value{
		"power.socket_power": {Unit: "W", Value: 10},
		"power.gfx_voltage":  {Unit: "mV", Value: 6},
		"power.soc_voltage":  {Unit: "mV", Value: 918},
		"power.mem_voltage":  {Unit: "mV", Value: 1250},
	})
}

func TestQuantityKeepsOnlyTheExpectedUnit(t *testing.T) {
	for name, tc := range map[string]struct {
		raw     any
		srcUnit string
		want    float64
		ok      bool
	}{
		"with unit":          {map[string]any{"value": 12.5, "unit": "W"}, "W", 12.5, true},
		"other unit":         {map[string]any{"value": 12.5, "unit": "mW"}, "W", 0, false},
		"bare count":         {float64(3), "", 3, true},
		"bare where unit":    {float64(3), "MB", 0, false},
		"N/A":                {"N/A", "%", 0, false},
		"value N/A":          {map[string]any{"value": "N/A", "unit": "%"}, "%", 0, false},
		"absent":             {nil, "", 0, false},
		"unit on bare field": {map[string]any{"value": 16.0, "unit": "x"}, "", 0, false},
	} {
		got, ok := quantity(tc.raw, tc.srcUnit)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("%s: quantity = %v, %v; want %v, %v", name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParseEntries(t *testing.T) {
	if got, err := parseEntries([]byte("{}\n")); err != nil || len(got) != 0 {
		t.Errorf("no GPU = %v, %v", got, err)
	}
	if _, err := parseEntries([]byte(`[{"usage": {}}]`)); err == nil {
		t.Error("an entry without a GPU number was accepted")
	}
	_, err := parseEntries([]byte("ERROR:root:Unable to get devices\nmore\n"))
	if err == nil || !strings.Contains(err.Error(), "Unable to get devices") || strings.Contains(err.Error(), "more") {
		t.Errorf("not JSON = %v, want its first line", err)
	}
}

// TestReadRetriesIdentityAfterFailure: amd-smi run without root fails on
// /dev/kfd; the scrape fails with amd-smi's message, and the next one tries
// the identity again.
func TestReadRetriesIdentityAfterFailure(t *testing.T) {
	s, _ := fakeSMI(t, "static-asic-v710-recorded.json", "list-v710-recorded.json", "metric-v710-recorded.json")
	recorded := s.run
	denied := errors.New("amd-smi static --asic --json: exit status 1: PermissionError: [Errno 13] Permission denied: '/dev/kfd'")
	calls := 0
	s.run = func(ctx context.Context, path string, args ...string) ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, denied
		}
		return recorded(ctx, path, args...)
	}
	if _, err := s.Read(context.Background()); err == nil || !strings.Contains(err.Error(), "/dev/kfd") {
		t.Fatalf("Read = %v, want the /dev/kfd error", err)
	}
	gpus, err := s.Read(context.Background())
	if err != nil || len(gpus) != 1 || gpus[0].Attributes["uuid"] == "" {
		t.Errorf("Read after recovery = %+v, %v", gpus, err)
	}
}

// TestRunCommandReportsStdout: an error amd-smi writes to standard output
// reaches the scrape error.
func TestRunCommandReportsStdout(t *testing.T) {
	_, err := runCommand(context.Background(), "sh", "-c", "echo \"Permission denied: '/dev/kfd'\"; exit 1")
	if err == nil || !strings.Contains(err.Error(), "Permission denied: '/dev/kfd'") {
		t.Errorf("runCommand = %v", err)
	}
}

func TestFindExecutable(t *testing.T) {
	if p, err := findExecutable("/x/amd-smi"); err != nil || p != "/x/amd-smi" {
		t.Errorf("a path = %q, %v", p, err)
	}
	if p, err := findExecutable("sh"); err != nil || !strings.HasSuffix(p, "/sh") {
		t.Errorf("on PATH = %q, %v", p, err)
	}
	t.Setenv("PATH", t.TempDir())
	saved := rocmPaths
	t.Cleanup(func() { rocmPaths = saved })
	dir := t.TempDir()
	rocm := dir + "/amd-smi"
	if err := os.WriteFile(rocm, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rocmPaths = []string{dir + "/missing", rocm}
	if p, err := findExecutable("amd-smi"); err != nil || p != rocm {
		t.Errorf("ROCm directory = %q, %v", p, err)
	}
	rocmPaths = []string{dir + "/missing"}
	if _, err := findExecutable("amd-smi"); err == nil {
		t.Error("a missing amd-smi was found")
	}
}

// TestRealAMDSMI runs the installed amd-smi when AIO11Y_AMD_SMI is set.
func TestRealAMDSMI(t *testing.T) {
	if os.Getenv("AIO11Y_AMD_SMI") == "" {
		t.Skip("AIO11Y_AMD_SMI is not set")
	}
	gpus, err := newAMDSMI(AMDSMIConfig{Path: "amd-smi"}).Read(context.Background())
	if err != nil || len(gpus) == 0 {
		t.Fatalf("Read = %+v, %v", gpus, err)
	}
	t.Logf("%d GPUs, first %v: %v", len(gpus), gpus[0].Attributes, gpus[0].Values)
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

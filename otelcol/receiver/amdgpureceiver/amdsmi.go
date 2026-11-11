package amdgpureceiver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// waitDelay bounds the wait for amd-smi's output once its context ended.
const waitDelay = 5 * time.Second

// rocmPaths are where ROCm installs amd-smi, looked at when a bare name is not
// on PATH.
var rocmPaths = []string{"/opt/rocm/bin/amd-smi"}

// runFunc runs amd-smi with args and returns its standard output.
type runFunc func(ctx context.Context, path string, args ...string) ([]byte, error)

// findExecutable resolves path: a path with a slash as it is, a bare name on
// PATH, then, for amd-smi, in the ROCm directory.
func findExecutable(path string) (string, error) {
	if strings.Contains(path, "/") {
		return path, nil
	}
	if p, err := exec.LookPath(path); err == nil {
		return p, nil
	}
	if path == "amd-smi" {
		for _, p := range rocmPaths {
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p, nil
			}
		}
		return "", fmt.Errorf("amd-smi is neither on PATH nor in %s", strings.Join(rocmPaths, ", "))
	}
	return "", fmt.Errorf("%s is not on PATH", path)
}

func runCommand(ctx context.Context, path string, args ...string) ([]byte, error) {
	bin, err := findExecutable(path)
	if err != nil {
		return nil, err
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// After the scrape timeout kills amd-smi, stop waiting for its output
	// shortly even when a process stuck in the driver keeps it open.
	cmd.WaitDelay = waitDelay
	if err := cmd.Run(); err != nil {
		// amd-smi writes some of its errors, such as a permission denied on
		// /dev/kfd when not run as root, to standard output.
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return nil, fmt.Errorf("%s %s: %w: %s", bin, strings.Join(args, " "), err, msg)
	}
	return stdout.Bytes(), nil
}

// identity is what names a GPU: its UUID (amd-smi list) and its product name
// (amd-smi static --asic).
type identity struct {
	uuid, name string
}

// amdSMI reads samples by running amd-smi metric --json. The identity of
// each GPU is read once, on the first scrape that gets it.
type amdSMI struct {
	path string
	run  runFunc

	identities map[int]identity
}

func newAMDSMI(cfg AMDSMIConfig) *amdSMI {
	return &amdSMI{path: cfg.Path, run: runCommand}
}

func (*amdSMI) Namespace() string { return SourceAMDSMI }

func (s *amdSMI) Read(ctx context.Context) ([]GPU, error) {
	ids, err := s.identity(ctx)
	if err != nil {
		return nil, err
	}
	out, err := s.run(ctx, s.path, "metric", "--json")
	if err != nil {
		return nil, err
	}
	entries, err := parseEntries(out)
	if err != nil {
		return nil, fmt.Errorf("amd-smi metric: %w", err)
	}
	gpus := make([]GPU, 0, len(entries))
	for _, e := range entries {
		attrs := map[string]string{"index": strconv.Itoa(e.id)}
		if id, ok := ids[e.id]; ok {
			putIf(attrs, "uuid", id.uuid)
			putIf(attrs, "name", id.name)
		}
		gpus = append(gpus, GPU{Attributes: attrs, Values: metricValues(e.fields)})
	}
	return gpus, nil
}

// identity reads the UUID and the product name of every GPU. A failure is
// not kept: the next scrape tries again, since the driver may come up later
// than the agent.
func (s *amdSMI) identity(ctx context.Context) (map[int]identity, error) {
	if s.identities != nil {
		return s.identities, nil
	}
	static, err := s.run(ctx, s.path, "static", "--asic", "--json")
	if err != nil {
		return nil, err
	}
	statics, err := parseEntries(static)
	if err != nil {
		return nil, fmt.Errorf("amd-smi static: %w", err)
	}
	list, err := s.run(ctx, s.path, "list", "--json")
	if err != nil {
		return nil, err
	}
	listed, err := parseEntries(list)
	if err != nil {
		return nil, fmt.Errorf("amd-smi list: %w", err)
	}
	ids := map[int]identity{}
	for _, e := range statics {
		id := ids[e.id]
		id.name = str(e.fields, "asic", "market_name")
		ids[e.id] = id
	}
	for _, e := range listed {
		id := ids[e.id]
		id.uuid = str(e.fields, "uuid")
		ids[e.id] = id
	}
	s.identities = ids
	return ids, nil
}

// entry is one GPU (or partition) of an amd-smi JSON document, its sections
// (usage, power, ...) by name.
type entry struct {
	id     int
	fields map[string]any
}

// parseEntries splits amd-smi's --json output into GPUs. The amd-smi of
// ROCm 6.1 writes a bare array; later ones wrap it as {"gpu_data": [...]},
// and write {} when there is no GPU.
func parseEntries(data []byte) ([]entry, error) {
	data = bytes.TrimSpace(data)
	var list []map[string]any
	switch {
	case bytes.HasPrefix(data, []byte("[")):
		if err := json.Unmarshal(data, &list); err != nil {
			return nil, fmt.Errorf("read JSON: %w", err)
		}
	case bytes.HasPrefix(data, []byte("{")):
		var wrapped struct {
			GPUData []map[string]any `json:"gpu_data"`
		}
		if err := json.Unmarshal(data, &wrapped); err != nil {
			return nil, fmt.Errorf("read JSON: %w", err)
		}
		list = wrapped.GPUData
	default:
		return nil, fmt.Errorf("output is not JSON: %q", firstLine(data))
	}
	out := make([]entry, 0, len(list))
	for _, m := range list {
		id, ok := m["gpu"].(float64)
		if !ok || id != math.Trunc(id) || id < 0 {
			return nil, fmt.Errorf("entry without a GPU number: %v", m["gpu"])
		}
		out = append(out, entry{id: int(id), fields: m})
	}
	return out, nil
}

// firstLine is the start of an answer that is not JSON, enough to tell what
// amd-smi said instead.
func firstLine(data []byte) string {
	line, _, _ := bytes.Cut(data, []byte("\n"))
	if len(line) > 200 {
		line = line[:200]
	}
	return string(line)
}

// field is a value of amd-smi metric read as a metric: its path in the
// document, the unit amd-smi writes for it (empty for a bare number) and the
// UCUM unit the metric carries.
type field struct {
	path    []string
	srcUnit string
	unit    string
}

// fields are the values of amd-smi metric read, in the order they are
// emitted. A value whose unit is not srcUnit is left out rather than sent in
// the wrong unit. amd-smi's "MB" is MiB: it divides bytes by 1024*1024.
var fields = []field{
	{[]string{"usage", "gfx_activity"}, "%", "%"},
	{[]string{"usage", "umc_activity"}, "%", "%"},
	{[]string{"usage", "mm_activity"}, "%", "%"},
	{[]string{"mem_usage", "total_vram"}, "MB", "MiBy"},
	{[]string{"mem_usage", "used_vram"}, "MB", "MiBy"},
	{[]string{"mem_usage", "free_vram"}, "MB", "MiBy"},
	{[]string{"mem_usage", "total_visible_vram"}, "MB", "MiBy"},
	{[]string{"mem_usage", "used_visible_vram"}, "MB", "MiBy"},
	{[]string{"mem_usage", "free_visible_vram"}, "MB", "MiBy"},
	{[]string{"mem_usage", "total_gtt"}, "MB", "MiBy"},
	{[]string{"mem_usage", "used_gtt"}, "MB", "MiBy"},
	{[]string{"mem_usage", "free_gtt"}, "MB", "MiBy"},
	{[]string{"power", "socket_power"}, "W", "W"},
	{[]string{"power", "gfx_voltage"}, "mV", "mV"},
	{[]string{"power", "soc_voltage"}, "mV", "mV"},
	{[]string{"power", "mem_voltage"}, "mV", "mV"},
	{[]string{"energy", "total_energy_consumption"}, "J", "J"},
	{[]string{"temperature", "edge"}, "C", "Cel"},
	{[]string{"temperature", "hotspot"}, "C", "Cel"},
	{[]string{"temperature", "mem"}, "C", "Cel"},
	{[]string{"pcie", "width"}, "", ""},
	{[]string{"pcie", "replay_count"}, "", ""},
	{[]string{"ecc", "total_correctable_count"}, "", ""},
	{[]string{"ecc", "total_uncorrectable_count"}, "", ""},
	{[]string{"ecc", "total_deferred_count"}, "", ""},
}

// metricValues reads the fields of one GPU of amd-smi metric, then the
// current clock of each clock domain (clock.gfx_0.clk, clock.mem_0.clk, ...).
// "N/A" and anything else that is not a number are left out.
func metricValues(m map[string]any) []Value {
	var out []Value
	for _, f := range fields {
		if v, ok := quantity(lookup(m, f.path...), f.srcUnit); ok {
			out = append(out, Value{Field: strings.Join(f.path, "."), Unit: f.unit, Value: v})
		}
	}
	if clocks, ok := m["clock"].(map[string]any); ok {
		for _, domain := range slices.Sorted(maps.Keys(clocks)) {
			if v, ok := quantity(lookup(clocks, domain, "clk"), "MHz"); ok {
				out = append(out, Value{Field: "clock." + domain + ".clk", Unit: "MHz", Value: v})
			}
		}
	}
	return out
}

// lookup takes the value at path in nested JSON objects, nil when absent.
func lookup(m map[string]any, path ...string) any {
	var cur any = m
	for _, k := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = obj[k]
	}
	return cur
}

// quantity reads one amd-smi value as a number in srcUnit: a bare number
// (counts, lane width) when srcUnit is empty, or {"value": n, "unit": u}
// with u equal to srcUnit.
func quantity(v any, srcUnit string) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, srcUnit == ""
	case map[string]any:
		n, ok := x["value"].(float64)
		unit, _ := x["unit"].(string)
		return n, ok && unit == srcUnit
	}
	return 0, false
}

// str is the string at path; "N/A" counts as absent.
func str(m map[string]any, path ...string) string {
	s, _ := lookup(m, path...).(string)
	if s = strings.TrimSpace(s); strings.EqualFold(s, "N/A") {
		return ""
	}
	return s
}

func putIf(m map[string]string, k, v string) {
	if v != "" {
		m[k] = v
	}
}

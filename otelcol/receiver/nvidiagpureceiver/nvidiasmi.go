package nvidiagpureceiver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// waitDelay bounds the wait for nvidia-smi's output once its context ended.
const waitDelay = 5 * time.Second

// identityFields are read with every sample and become attributes.
var identityFields = []string{"index", "uuid", "name"}

// busIDField is read with every sample to find a GPU by its PCI address, which
// XID lines name it by. It is not an attribute.
const busIDField = "pci.bus_id"

// runFunc runs nvidia-smi with args and returns its standard output.
type runFunc func(ctx context.Context, path string, args ...string) ([]byte, error)

func runCommand(ctx context.Context, path string, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stderr = &stderr
	// After the scrape timeout kills nvidia-smi, stop waiting for its output
	// shortly even when a process stuck in the driver keeps it open.
	cmd.WaitDelay = waitDelay
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", path, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// nvidiaSMI reads samples by running nvidia-smi --query-gpu.
type nvidiaSMI struct {
	path   string
	fields []string
	run    runFunc

	once      sync.Once
	queryErr  error
	supported []string
}

func newNvidiaSMI(cfg NvidiaSMIConfig) *nvidiaSMI {
	return &nvidiaSMI{path: cfg.Path, fields: cfg.Fields, run: runCommand}
}

func (*nvidiaSMI) Namespace() string { return SourceNvidiaSMI }

// supportedFields keeps the configured fields the installed nvidia-smi knows:
// an unknown field fails the whole query.
func (s *nvidiaSMI) supportedFields(ctx context.Context) ([]string, error) {
	s.once.Do(func() {
		out, err := s.run(ctx, s.path, "--help-query-gpu")
		if err != nil {
			s.queryErr = err
			return
		}
		known := helpFields(out)
		for _, f := range s.fields {
			if known[f] && !slices.Contains(s.supported, f) {
				s.supported = append(s.supported, f)
			}
		}
	})
	if s.queryErr != nil {
		// Try again next time: nvidia-smi may come up later than the agent.
		err := s.queryErr
		s.once, s.queryErr = sync.Once{}, nil
		return nil, err
	}
	return s.supported, nil
}

// helpFields parses `nvidia-smi --help-query-gpu`: each field is a line
// starting with its quoted name, aliases joined by "or".
func helpFields(out []byte) map[string]bool {
	known := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, `"`) {
			continue
		}
		for _, part := range strings.Split(line, " or ") {
			if name := strings.Trim(strings.TrimSpace(part), `"`); name != "" {
				known[name] = true
			}
		}
	}
	return known
}

func (s *nvidiaSMI) Read(ctx context.Context) ([]GPU, error) {
	fields, err := s.supportedFields(ctx)
	if err != nil {
		return nil, err
	}
	all := append(append(slices.Clone(identityFields), busIDField), fields...)
	out, err := s.run(ctx, s.path, "--query-gpu="+strings.Join(all, ","), "--format=csv,nounits")
	if err != nil {
		return nil, err
	}
	return parseQuery(out, all)
}

// parseQuery reads nvidia-smi's CSV answer to a query for fields. The header
// carries each field's unit in brackets ("memory.used [MiB]"); the field
// names come from fields, since nvidia-smi writes some under another name
// (clocks.sm as clocks.current.sm).
func parseQuery(out []byte, fields []string) ([]GPU, error) {
	r := csv.NewReader(bytes.NewReader(out))
	r.TrimLeadingSpace = true
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi answered no header: %w", err)
	}
	if len(header) != len(fields) {
		return nil, fmt.Errorf("nvidia-smi answered %d columns for %d fields", len(header), len(fields))
	}
	units := make([]string, len(fields))
	for i, h := range header {
		units[i] = unitOf(fields[i], h)
	}
	var gpus []GPU
	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("nvidia-smi answer: %w", err)
		}
		if len(row) != len(fields) {
			return nil, fmt.Errorf("nvidia-smi answered a row of %d columns for %d fields", len(row), len(fields))
		}
		g := GPU{Attributes: map[string]string{}}
		for i, f := range fields {
			cell := strings.TrimSpace(row[i])
			if slices.Contains(identityFields, f) {
				g.Attributes[f] = cell
				continue
			}
			if f == busIDField {
				g.PCIBusID = cell
				continue
			}
			if v, ok := parseValue(cell); ok {
				g.Values = append(g.Values, Value{Field: f, Unit: units[i], Value: v})
			}
		}
		gpus = append(gpus, g)
	}
	return gpus, nil
}

// units maps nvidia-smi's header units to UCUM.
var units = map[string]string{"%": "%", "MiB": "MiBy", "W": "W", "MHz": "MHz", "J": "J", "mJ": "mJ", "C": "Cel"}

func unitOf(field, header string) string {
	if i, j := strings.LastIndex(header, "["), strings.LastIndex(header, "]"); i >= 0 && j > i {
		if u, ok := units[header[i+1:j]]; ok {
			return u
		}
		return ""
	}
	// nvidia-smi gives temperatures in degrees Celsius without a unit.
	if strings.HasPrefix(field, "temperature.") {
		return "Cel"
	}
	return ""
}

// parseValue reads a cell as a number: plain numbers, P-states (P8 is 8),
// hex bit masks, and Enabled/Yes (1) or Disabled/No (0). "[N/A]",
// "[Not Supported]" and anything else are not values.
func parseValue(cell string) (float64, bool) {
	switch strings.ToLower(cell) {
	case "enabled", "yes", "active":
		return 1, true
	case "disabled", "no", "not active":
		return 0, true
	}
	if strings.HasPrefix(cell, "P") {
		if n, err := strconv.Atoi(cell[1:]); err == nil {
			return float64(n), true
		}
	}
	if strings.HasPrefix(cell, "0x") {
		if n, err := strconv.ParseUint(cell[2:], 16, 64); err == nil {
			return float64(n), true
		}
	}
	if v, err := strconv.ParseFloat(cell, 64); err == nil {
		return v, true
	}
	return 0, false
}

// Package collector holds the CollectorKind providers of the agent path and
// the renderer that merges their fragments into one collector configuration.
package collector

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/cloud-barista/ai-o11y/internal/monitoring"
	"github.com/cloud-barista/ai-o11y/internal/monitoring/catalog"
)

// DefaultInterval matches the collection interval of the current host
// monitoring.
const DefaultInterval = 30 * time.Second

// ErrHealthUnsupported is returned by Health until collectors report their
// own state back to ai-o11y.
var ErrHealthUnsupported = errors.New("collector health is not reported yet")

// HostmetricsReceiver is the receiver id the hostmetrics kind renders. The
// collector release in use renamed "hostmetrics" to "host_metrics".
const HostmetricsReceiver = "host_metrics"

// TopProcessProcessor is the processor id of ai-o11y's top process processor
// (otelcol/processor/topprocessprocessor). It keeps the per-process metrics of
// the TopProcesses processes using the most CPU and of the TopProcesses using
// the most memory, and copies process.executable.name and process.pid onto
// their data points so every backend labels the series with them.
const TopProcessProcessor = "top_process"

// TopProcesses is how many processes each of CPU and memory keeps.
const TopProcesses = 10

// processScraperMutes are the process scraper's errors for processes it
// cannot read, such as one that exits during the scrape or, without root,
// another user's. They are expected on every scrape and would fill the log.
var processScraperMutes = []string{
	"mute_process_name_error",
	"mute_process_exe_error",
	"mute_process_io_error",
	"mute_process_user_error",
	"mute_process_cgroup_error",
}

// scraperPrefixes maps a hostmetrics metric name prefix to the scraper that
// emits it. Longer prefixes come first so system_cpu_load_average resolves to
// load, not cpu. Prefixes are written with underscores because the dotted
// names keep underscores of their own (load_average), so only
// dots-to-underscores is lossless; normalize() maps dotted names over.
var scraperPrefixes = []struct{ prefix, scraper string }{
	{"system_cpu_load_average", "load"},
	{"system_cpu_", "cpu"},
	{"system_linux_memory_", "memory"},
	{"system_memory_", "memory"},
	{"system_paging_", "paging"},
	{"system_filesystem_", "filesystem"},
	{"system_disk_", "disk"},
	{"system_network_", "network"},
	{"system_processes_", "processes"},
	{"system_uptime", "system"},
	{"process_", "process"},
}

// metricNames returns the names of the series a catalog expression selects.
// It parses the expression, so label names such as process_state are not
// taken for series.
func metricNames(expr string) ([]string, error) {
	filled := strings.NewReplacer(
		monitoring.PlaceholderSelector, `entity_id="x"`,
		monitoring.PlaceholderRange, "1m",
	).Replace(expr)
	node, err := parser.NewParser(parser.Options{}).ParseExpr(filled)
	if err != nil {
		return nil, err
	}
	var names []string
	parser.Inspect(node, func(n parser.Node, _ []parser.Node) error {
		vs, ok := n.(*parser.VectorSelector)
		if !ok {
			return nil
		}
		if vs.Name != "" {
			names = append(names, vs.Name)
			return nil
		}
		// A quoted name, {"system.cpu.time"}, is a __name__ matcher.
		for _, m := range vs.LabelMatchers {
			if m.Name == model.MetricNameLabel && m.Type == labels.MatchEqual {
				names = append(names, m.Value)
			}
		}
		return nil
	})
	return names, nil
}

func normalize(name string) string { return strings.ReplaceAll(name, ".", "_") }

func scraperOf(name string) (string, error) {
	n := normalize(name)
	for _, p := range scraperPrefixes {
		if strings.HasPrefix(n, p.prefix) {
			return p.scraper, nil
		}
	}
	return "", fmt.Errorf("no hostmetrics scraper emits %q", name)
}

// Hostmetrics renders the hostmetrics receiver for a VM: the scrapers the
// catalog's hostmetrics expressions read from, with the optional metrics those
// rows need turned on.
type Hostmetrics struct {
	Interval time.Duration
}

var _ monitoring.CollectorKind = Hostmetrics{}

// Path implements monitoring.CollectorKind.
func (Hostmetrics) Path() monitoring.Path { return monitoring.PathAgent }

// Name implements monitoring.CollectorKind.
func (Hostmetrics) Name() string { return catalog.CollectorHostmetrics }

// AppliesTo implements monitoring.CollectorKind: every VM runs hostmetrics.
func (Hostmetrics) AppliesTo(t monitoring.Target) bool { return t.Kind == "vm" }

// Render implements monitoring.CollectorKind. Rows with a gap are skipped. An
// expression or enabled metric no scraper is known to emit is an error, so a
// new catalog row cannot silently collect nothing. When a row needs the
// process scraper, its per-process read errors are muted and the top process
// processor (TopProcessProcessor) is rendered, so only the busiest processes
// are sent.
func (h Hostmetrics) Render(t monitoring.Target, c monitoring.MetricCatalog) (monitoring.ConfigFragment, error) {
	interval := h.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	scrapers := map[string]map[string]any{}
	for _, row := range c.Rows() {
		expr, ok := row.Exprs[catalog.CollectorHostmetrics]
		if !ok || row.Gap != "" {
			continue
		}
		var names []string
		for _, e := range []string{expr, row.SplitExprs[catalog.CollectorHostmetrics], row.ZeroFill[catalog.CollectorHostmetrics]} {
			if e == "" {
				continue
			}
			found, err := metricNames(e)
			if err != nil {
				return monitoring.ConfigFragment{}, fmt.Errorf("catalog row %s: %w", row.ID(), err)
			}
			if len(found) == 0 {
				return monitoring.ConfigFragment{}, fmt.Errorf("catalog row %s: expression selects no series by name", row.ID())
			}
			names = append(names, found...)
		}
		for _, name := range names {
			s, err := scraperOf(name)
			if err != nil {
				return monitoring.ConfigFragment{}, fmt.Errorf("catalog row %s: %w", row.Name, err)
			}
			if scrapers[s] == nil {
				scrapers[s] = map[string]any{}
			}
		}
		for _, metric := range row.Enables {
			s, err := scraperOf(metric)
			if err != nil {
				return monitoring.ConfigFragment{}, fmt.Errorf("catalog row %s: %w", row.Name, err)
			}
			if scrapers[s] == nil {
				scrapers[s] = map[string]any{}
			}
			metrics, _ := scrapers[s]["metrics"].(map[string]any)
			if metrics == nil {
				metrics = map[string]any{}
				scrapers[s]["metrics"] = metrics
			}
			metrics[metric] = map[string]any{"enabled": true}
		}
	}
	if len(scrapers) == 0 {
		return monitoring.ConfigFragment{}, errors.New("catalog has no hostmetrics expression")
	}

	var processors map[string]any
	if process, ok := scrapers["process"]; ok {
		for _, mute := range processScraperMutes {
			process[mute] = true
		}
		processors = map[string]any{TopProcessProcessor: map[string]any{
			"top_n": TopProcesses,
			"by":    []string{"cpu", "memory"},
		}}
	}

	scraperCfg := make(map[string]any, len(scrapers))
	for name, cfg := range scrapers {
		scraperCfg[name] = cfg
	}
	return monitoring.ConfigFragment{
		Receivers: map[string]any{
			HostmetricsReceiver: map[string]any{
				"collection_interval": interval.String(),
				"scrapers":            scraperCfg,
			},
		},
		Processors: processors,
	}, nil
}

// Health implements monitoring.CollectorKind.
func (Hostmetrics) Health(context.Context, monitoring.Target) (monitoring.CollectorHealth, error) {
	return monitoring.CollectorHealth{}, ErrHealthUnsupported
}

package collector

import (
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/cloud-barista/ai-o11y/internal/monitoring"
)

// BatchProcessor is the processor that groups data points before they are
// sent. It runs with its defaults: it sends what it has 200ms after the first
// point arrives, so a collector still sends right after each collection and
// the send times of different collectors stay as spread as their collection
// times.
const BatchProcessor = "batch"

// MemoryLimiterProcessor is the processor that refuses data points while the
// collector's memory use is over a limit, so a backlog makes it drop samples
// rather than be killed. It runs first, as the collector documentation
// recommends.
const MemoryLimiterProcessor = "memory_limiter"

// MemoryLimitMiB and MemorySpikeLimitMiB are the memory limiter's hard limit
// and the headroom below it where it starts refusing data. They stay under
// the MemoryMax=512M of the installed service unit
// (otelcol/install/aio11y-otelcol.service), so the limiter acts before the
// kernel kills the collector.
const (
	MemoryLimitMiB      = 400
	MemorySpikeLimitMiB = 100
)

// ExtensionQueueStorage is the file storage extension the disk queue is kept
// in.
const ExtensionQueueStorage = "file_storage/queue"

// DefaultStorageDir is where the disk queue is kept when Options.StorageDir is
// empty: under the state directory of the installed service unit.
const DefaultStorageDir = "/var/lib/aio11y-otelcol/queue"

// QueueSizeBytes bounds the disk queue, counted in bytes of encoded data
// points: while it is full, new data points are dropped.
const QueueSizeBytes = 256 << 20

// ExporterOTLPHTTP is the exporter id the renderer writes. The collector
// release in use renamed "otlphttp" to "otlp_http".
const ExporterOTLPHTTP = "otlp_http"

// ConnectorFailover is the connector that sends to the first write endpoint
// and moves to the next while it fails, used when there are several.
const ConnectorFailover = "failover"

// ExtensionBasicAuth is the extension that signs an exporter's requests with
// a basic auth account.
const ExtensionBasicAuth = "basicauth"

// JournaldReceiver is the receiver the logs pipeline reads the host's
// systemd journal with. It runs journalctl, so it needs the journal readable
// by the collector's user (root in the installed service unit).
const JournaldReceiver = "journald"

// ExtensionJournaldStorage is the file storage extension the journald
// receiver keeps its cursor in, so a restarted collector reads on from where
// it stopped instead of from the end.
const ExtensionJournaldStorage = "file_storage/journald"

// OTLPReceiver is the receiver the traces pipeline takes spans with, from the
// applications on the collector's host. It listens on localhost only:
// OTLPGRPCEndpoint and OTLPHTTPEndpoint are the OTLP default ports.
const (
	OTLPReceiver     = "otlp"
	OTLPGRPCEndpoint = "127.0.0.1:4317"
	OTLPHTTPEndpoint = "127.0.0.1:4318"
)

// WritePasswordEnv is the environment variable on a collector's host that
// holds the password of the write account (AIO11Y_BACKEND_WRITE_USERNAME).
const WritePasswordEnv = "AIO11Y_WRITE_PASSWORD"

// Render builds the collector configuration for one target: the fragments of
// every agent-path kind that applies to it, the processor that stamps its
// identity labels, the memory limiter, and one metrics pipeline to the
// endpoints: one endpoint as one exporter, several behind a failover connector
// (see exportTo). Two
// fragments that define the same component id are an error rather than one
// silently replacing the other. The output is YAML with sorted keys, so the same
// inputs always render the same bytes.
//
// Options.LogsEndpoints adds a logs pipeline from the systemd journal, and
// Options.TracesEndpoints a traces pipeline from an OTLP receiver on
// localhost; each goes to its endpoints by the same rules as the metrics, with
// a disk queue of its own. A Windows target gets no logs pipeline: it has no
// journal, and its event log is not read yet.
func Render(t monitoring.Target, kinds []monitoring.CollectorKind, c monitoring.MetricCatalog, endpoints []monitoring.Endpoint, opts Options) ([]byte, error) {
	if len(endpoints) == 0 {
		return nil, errors.New("collector has no endpoint")
	}
	for _, set := range [][]monitoring.Endpoint{endpoints, opts.LogsEndpoints, opts.TracesEndpoints} {
		for _, e := range set {
			if e.URL == "" {
				return nil, errors.New("collector endpoint has no URL")
			}
			if a := e.BasicAuth; a != nil && (a.Username == "" || a.PasswordEnv == "") {
				return nil, errors.New("collector endpoint account needs a username and a password variable")
			}
		}
	}
	identity, err := monitoring.IdentityFragment(t)
	if err != nil {
		return nil, err
	}

	receivers := map[string]any{}
	processors := map[string]any{}
	for _, k := range kinds {
		if k.Path() != monitoring.PathAgent || !k.AppliesTo(t) {
			continue
		}
		frag, err := k.Render(t, c)
		if err != nil {
			return nil, fmt.Errorf("render target %q: %w", t.EntityID, err)
		}
		if err := mergeInto(receivers, frag.Receivers, "receiver"); err != nil {
			return nil, err
		}
		if err := mergeInto(processors, frag.Processors, "processor"); err != nil {
			return nil, err
		}
	}
	if len(receivers) == 0 {
		return nil, fmt.Errorf("no collector kind applies to target %q", t.EntityID)
	}
	if err := mergeInto(processors, identity.Processors, "processor"); err != nil {
		return nil, err
	}

	// The memory limiter runs first, so data points are refused before any
	// other processor holds them. The identity processor runs after every
	// other processor that may change data points, so none can change the
	// label it stamps; batch only groups them and goes last, as the collector
	// documentation recommends. A processor that compares the data points of
	// one collection, such as the top process processor, so sees one
	// collection at a time.
	limiter := map[string]any{
		"check_interval":  "1s",
		"limit_mib":       MemoryLimitMiB,
		"spike_limit_mib": MemorySpikeLimitMiB,
	}
	if err := mergeInto(processors, map[string]any{MemoryLimiterProcessor: limiter, BatchProcessor: map[string]any{}}, "processor"); err != nil {
		return nil, err
	}
	pipelineProcessors := slices.Sorted(maps.Keys(processors))
	pipelineProcessors = slices.DeleteFunc(pipelineProcessors, func(p string) bool {
		return p == MemoryLimiterProcessor || p == monitoring.IdentityProcessor || p == BatchProcessor
	})
	pipelineProcessors = append([]string{MemoryLimiterProcessor}, pipelineProcessors...)
	pipelineProcessors = append(pipelineProcessors, monitoring.IdentityProcessor, BatchProcessor)

	storageDir := opts.StorageDir
	if storageDir == "" {
		storageDir = DefaultStorageDir
	}
	signalProcessors := []string{MemoryLimiterProcessor, monitoring.IdentityProcessor, BatchProcessor}
	signals := []signal{{
		name:       "metrics",
		receivers:  slices.Sorted(maps.Keys(receivers)),
		processors: pipelineProcessors,
		endpoints:  endpoints,
		storageDir: storageDir,
	}}
	extensions := map[string]any{}
	if len(opts.LogsEndpoints) > 0 && !strings.EqualFold(t.OS, "windows") {
		journald := map[string]any{
			"start_at":                        "end",
			"storage":                         ExtensionJournaldStorage,
			"convert_to_semantic_conventions": true,
		}
		if err := mergeInto(receivers, map[string]any{JournaldReceiver: journald}, "receiver"); err != nil {
			return nil, err
		}
		extensions[ExtensionJournaldStorage] = map[string]any{
			"directory":        path.Join(storageDir, "journald"),
			"create_directory": true,
		}
		signals = append(signals, signal{
			name:       "logs",
			receivers:  []string{JournaldReceiver},
			processors: signalProcessors,
			endpoints:  opts.LogsEndpoints,
			storageDir: path.Join(storageDir, "logs"),
		})
	}
	if len(opts.TracesEndpoints) > 0 {
		otlp := map[string]any{"protocols": map[string]any{
			"grpc": map[string]any{"endpoint": OTLPGRPCEndpoint},
			"http": map[string]any{"endpoint": OTLPHTTPEndpoint},
		}}
		if err := mergeInto(receivers, map[string]any{OTLPReceiver: otlp}, "receiver"); err != nil {
			return nil, err
		}
		signals = append(signals, signal{
			name:       "traces",
			receivers:  []string{OTLPReceiver},
			processors: signalProcessors,
			endpoints:  opts.TracesEndpoints,
			storageDir: path.Join(storageDir, "traces"),
		})
	}

	pipelines := map[string]any{}
	exporters := map[string]any{}
	connectors := map[string]any{}
	for _, sig := range signals {
		out := exportTo(sig, opts.TLS)
		pipelines[sig.name] = map[string]any{
			"receivers":  sig.receivers,
			"processors": sig.processors,
			"exporters":  []string{out.head},
		}
		maps.Copy(pipelines, out.pipelines)
		maps.Copy(exporters, out.exporters)
		maps.Copy(connectors, out.connectors)
		maps.Copy(extensions, out.extensions)
	}
	cfg := map[string]any{
		"receivers":  receivers,
		"processors": processors,
		"exporters":  exporters,
		"service": map[string]any{
			"telemetry": telemetry(opts.TelemetryPort),
			"pipelines": pipelines,
		},
	}
	if len(connectors) > 0 {
		cfg["connectors"] = connectors
	}
	if len(extensions) > 0 {
		cfg["extensions"] = extensions
		cfg["service"].(map[string]any)["extensions"] = slices.Sorted(maps.Keys(extensions))
	}
	return yaml.Marshal(cfg)
}

// signal is one pipeline the renderer writes and where it goes: name is the
// pipeline, "metrics", "logs" or "traces", and storageDir the directory of its
// disk queue, one per signal so no two share one.
type signal struct {
	name       string
	receivers  []string
	processors []string
	endpoints  []monitoring.Endpoint
	storageDir string
}

// id is the component id of kind for the signal: the metrics keep the ids
// they had before the other signals came, kind for one endpoint and kind/i
// for the i-th of several; another signal adds its name, kind/logs and
// kind/logs_i.
func (s signal) id(kind string, i int) string {
	switch {
	case s.name == "metrics" && i < 0:
		return kind
	case s.name == "metrics":
		return fmt.Sprintf("%s/%d", kind, i)
	case i < 0:
		return kind + "/" + s.name
	default:
		return fmt.Sprintf("%s/%s_%d", kind, s.name, i)
	}
}

// queueStorage is the file storage extension id of the signal's disk queue.
func (s signal) queueStorage() string {
	if s.name == "metrics" {
		return ExtensionQueueStorage
	}
	return ExtensionQueueStorage + "_" + s.name
}

// output is where a signal's pipeline ends: head is the component it exports
// to, with the exporters, connectors, extensions and extra pipelines that come
// with it.
type output struct {
	head       string
	exporters  map[string]any
	connectors map[string]any
	extensions map[string]any
	pipelines  map[string]any
}

// exportTo writes the signal's endpoints. One endpoint is one exporter that
// keeps what it cannot send in the disk queue under the signal's storage
// directory (see keepOnDisk). Several
// are one exporter each, in a pipeline per priority behind the failover
// connector, as the connector documentation sets them up: every exporter but
// the last has its queue and retries off, so a failure reaches the connector,
// which then moves to the next endpoint and goes back to the first once it
// answers again. The last keeps what it cannot send on disk, so data is
// kept while every endpoint fails.
func exportTo(sig signal, tls TLS) output {
	queue := map[string]any{"directory": sig.storageDir, "create_directory": true}
	storage := sig.queueStorage()
	if len(sig.endpoints) == 1 {
		exporter := sig.id(ExporterOTLPHTTP, -1)
		out := output{head: exporter, extensions: map[string]any{storage: queue}}
		cfg := exporterConfig(sig.endpoints[0], sig.id(ExtensionBasicAuth, -1), tls, out.extensions)
		keepOnDisk(cfg, storage)
		out.exporters = map[string]any{exporter: cfg}
		return out
	}
	connector := sig.id(ConnectorFailover, -1)
	out := output{head: connector, exporters: map[string]any{}, extensions: map[string]any{}, pipelines: map[string]any{}}
	levels := make([]any, 0, len(sig.endpoints))
	for i, e := range sig.endpoints {
		exporter := sig.id(ExporterOTLPHTTP, i)
		pipeline := fmt.Sprintf("%s/%d", sig.name, i)
		cfg := exporterConfig(e, sig.id(ExtensionBasicAuth, i), tls, out.extensions)
		if i == len(sig.endpoints)-1 {
			keepOnDisk(cfg, storage)
			out.extensions[storage] = queue
		} else {
			cfg["sending_queue"] = map[string]any{"enabled": false}
			cfg["retry_on_failure"] = map[string]any{"enabled": false}
		}
		out.exporters[exporter] = cfg
		out.pipelines[pipeline] = map[string]any{
			"receivers": []string{connector},
			"exporters": []string{exporter},
		}
		levels = append(levels, []string{pipeline})
	}
	out.connectors = map[string]any{connector: map[string]any{"priority_levels": levels}}
	return out
}

// keepOnDisk makes the exporter cfg queue what it sends in the disk queue
// kept by the storage extension and retry a failed send until it goes through
// (max_elapsed_time 0), so samples outlive a backend outage and a collector
// restart, up to QueueSizeBytes. One consumer sends them one request at a
// time in the order they were queued, so the backend gets each series' samples
// in time order. Only across a restart they are not: the queue puts the
// request that was being sent behind the others.
func keepOnDisk(cfg map[string]any, storage string) {
	cfg["sending_queue"] = map[string]any{
		"enabled":       true,
		"storage":       storage,
		"num_consumers": 1,
		"sizer":         "bytes",
		"queue_size":    QueueSizeBytes,
	}
	cfg["retry_on_failure"] = map[string]any{"enabled": true, "max_elapsed_time": "0s"}
}

// exporterConfig is the exporter for e, verifying https endpoints as tls
// says. An account adds a basic auth
// extension under authID to extensions, its password taken from the
// collector host's environment when the collector starts.
func exporterConfig(e monitoring.Endpoint, authID string, tls TLS, extensions map[string]any) map[string]any {
	cfg := map[string]any{"endpoint": e.URL}
	if len(e.Headers) > 0 {
		cfg["headers"] = maps.Clone(e.Headers)
	}
	if t := tls.config(); t != nil {
		cfg["tls"] = t
	}
	if a := e.BasicAuth; a != nil {
		extensions[authID] = map[string]any{"client_auth": map[string]any{
			"username": a.Username,
			"password": "${env:" + a.PasswordEnv + "}",
		}}
		cfg["auth"] = map[string]any{"authenticator": authID}
	}
	return cfg
}

func mergeInto(dst, src map[string]any, kind string) error {
	for id, v := range src {
		if _, ok := dst[id]; ok {
			return fmt.Errorf("%s %q is defined by two collector kinds", kind, id)
		}
		dst[id] = v
	}
	return nil
}

// Options are deployment-wide settings of the rendered configuration.
type Options struct {
	// TelemetryPort is the localhost port the collector serves its own
	// metrics on (Prometheus format). Zero turns them off, so the collector
	// opens no port another collector on the host may already hold; nothing
	// reads them yet.
	TelemetryPort int
	// StorageDir is the directory on the collector's host its disk queue is
	// kept in; the collector creates it. Empty is DefaultStorageDir. The logs
	// and traces queues and the journal cursor are kept in its
	// subdirectories logs, traces and journald.
	StorageDir string
	// LogsEndpoints are where the host's systemd journal is sent, in order of
	// preference as the metrics endpoints are. Empty renders no logs
	// pipeline.
	LogsEndpoints []monitoring.Endpoint
	// TracesEndpoints are where spans the host's applications send to the
	// collector's OTLP receiver go, in order of preference. Empty renders no
	// traces pipeline and opens no OTLP port.
	TracesEndpoints []monitoring.Endpoint
	// TLS is how every exporter verifies an https endpoint.
	TLS TLS
}

// TLS is how the collector verifies the certificate of an https endpoint.
// The zero value verifies against the system's CAs.
type TLS struct {
	// CAFile is a PEM file of CAs on the collector's host to verify against
	// instead of the system's. Only the path is rendered; the collector reads
	// the file when it starts.
	CAFile string
	// InsecureSkipVerify accepts any certificate. For trying out a setup
	// only.
	InsecureSkipVerify bool
}

// config is the exporter's tls section, nil for the zero value so a
// configuration without TLS settings renders as before.
func (t TLS) config() map[string]any {
	cfg := map[string]any{}
	if t.CAFile != "" {
		cfg["ca_file"] = t.CAFile
	}
	if t.InsecureSkipVerify {
		cfg["insecure_skip_verify"] = true
	}
	if len(cfg) == 0 {
		return nil
	}
	return cfg
}

// telemetry is the collector's own telemetry section.
func telemetry(port int) map[string]any {
	if port <= 0 {
		return map[string]any{"metrics": map[string]any{"level": "none"}}
	}
	return map[string]any{"metrics": map[string]any{
		"level": "basic",
		"readers": []any{map[string]any{"pull": map[string]any{"exporter": map[string]any{
			"prometheus": map[string]any{"host": "127.0.0.1", "port": port},
		}}}},
	}}
}

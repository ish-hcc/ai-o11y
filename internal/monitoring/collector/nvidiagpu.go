package collector

import (
	"context"
	"strings"
	"time"

	"github.com/cloud-barista/ai-o11y/internal/monitoring"
	"github.com/cloud-barista/ai-o11y/internal/monitoring/catalog"
)

// NvidiaGPUReceiver is the receiver id of ai-o11y's NVIDIA GPU receiver
// (otelcol/receiver/nvidiagpureceiver).
const NvidiaGPUReceiver = "nvidia_gpu"

// NvidiaGPU renders the NVIDIA GPU receiver, reading nvidia-smi, for a VM with
// GPUs. It also counts the XID errors the driver logs to the kernel log
// (/dev/kmsg), which the collector can read since it runs as root.
type NvidiaGPU struct {
	Interval time.Duration
}

var _ monitoring.CollectorKind = NvidiaGPU{}

// Name implements monitoring.CollectorKind.
func (NvidiaGPU) Name() string { return catalog.CollectorNvidiaSMI }

// Path implements monitoring.CollectorKind.
func (NvidiaGPU) Path() monitoring.Path { return monitoring.PathAgent }

// AppliesTo implements monitoring.CollectorKind: a VM the platform reports
// NVIDIA GPUs on. The vendor is known from the GPU model, which CB-Tumblebug
// names with it ("NVIDIA T4", "nvidia-a100-80gb"); a model that does not name
// NVIDIA, or none, gets no GPU receiver rather than a guess. The count may be
// unknown (0) while the model is not: see hasGPU.
func (NvidiaGPU) AppliesTo(t monitoring.Target) bool {
	return t.Kind == "vm" && hasGPU(t) && strings.Contains(strings.ToLower(t.GPUModel), "nvidia")
}

// hasGPU reports whether the platform reports GPUs on t: a count, or a model
// alone. CB-Tumblebug leaves the count of some specs out (acceleratorCount is
// null for Azure Standard_NV4ads_V710_v5) while naming the model, and the
// tumblebug source fills the model only for a spec whose accelerator is a GPU.
func hasGPU(t monitoring.Target) bool {
	return t.GPUCount > 0 || t.GPUModel != ""
}

// Render implements monitoring.CollectorKind.
func (g NvidiaGPU) Render(monitoring.Target, monitoring.MetricCatalog) (monitoring.ConfigFragment, error) {
	interval := g.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	return monitoring.ConfigFragment{
		Receivers: map[string]any{
			NvidiaGPUReceiver: map[string]any{
				"collection_interval": interval.String(),
				"source":              catalog.CollectorNvidiaSMI,
				"xid":                 map[string]any{"enabled": true},
			},
		},
	}, nil
}

// Health implements monitoring.CollectorKind.
func (NvidiaGPU) Health(context.Context, monitoring.Target) (monitoring.CollectorHealth, error) {
	return monitoring.CollectorHealth{}, ErrHealthUnsupported
}

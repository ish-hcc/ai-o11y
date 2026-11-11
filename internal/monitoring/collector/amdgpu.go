package collector

import (
	"context"
	"strings"
	"time"

	"github.com/cloud-barista/ai-o11y/internal/monitoring"
	"github.com/cloud-barista/ai-o11y/internal/monitoring/catalog"
)

// AMDGPUReceiver is the receiver id of ai-o11y's AMD GPU receiver
// (otelcol/receiver/amdgpureceiver).
const AMDGPUReceiver = "amd_gpu"

// AMDGPU renders the AMD GPU receiver, reading amd-smi, for a VM with GPUs.
type AMDGPU struct {
	Interval time.Duration
}

var _ monitoring.CollectorKind = AMDGPU{}

// Name implements monitoring.CollectorKind.
func (AMDGPU) Name() string { return catalog.CollectorAMDSMI }

// Path implements monitoring.CollectorKind.
func (AMDGPU) Path() monitoring.Path { return monitoring.PathAgent }

// AppliesTo implements monitoring.CollectorKind: a VM the platform reports
// AMD GPUs on. CB-Tumblebug names the vendor in the model ("AMD RADEON PRO
// V710", "AMD MI300X"), or at least the product line ("AMD INSTINCT").
func (AMDGPU) AppliesTo(t monitoring.Target) bool {
	if t.Kind != "vm" || !hasGPU(t) {
		return false
	}
	model := strings.ToLower(t.GPUModel)
	return strings.Contains(model, "amd") || strings.Contains(model, "radeon") || strings.Contains(model, "instinct")
}

// Render implements monitoring.CollectorKind.
func (g AMDGPU) Render(monitoring.Target, monitoring.MetricCatalog) (monitoring.ConfigFragment, error) {
	interval := g.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	return monitoring.ConfigFragment{
		Receivers: map[string]any{
			AMDGPUReceiver: map[string]any{
				"collection_interval": interval.String(),
				"source":              catalog.CollectorAMDSMI,
			},
		},
	}, nil
}

// Health implements monitoring.CollectorKind.
func (AMDGPU) Health(context.Context, monitoring.Target) (monitoring.CollectorHealth, error) {
	return monitoring.CollectorHealth{}, ErrHealthUnsupported
}

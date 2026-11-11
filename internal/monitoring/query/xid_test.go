package query

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/promqltest"

	"github.com/cloud-barista/ai-o11y/internal/monitoring"
)

// TestXIDErrors runs nvidia_kmsg:xid.errors on series as the nvidia_gpu
// receiver writes them: one per GPU and XID number, and one by the kernel's
// PCI address for a GPU nvidia-smi did not name.
func TestXIDErrors(t *testing.T) {
	st := promqltest.LoadedStorage(t, `load 30s
  {__name__="nvidia_kmsg.xid.errors",entity_id="a",uuid="GPU-1",xid="79"} 1 1 2x18
  {__name__="nvidia_kmsg.xid.errors",entity_id="a",uuid="GPU-1",xid="31"} 1x20
  {__name__="nvidia_kmsg.xid.errors",entity_id="a","pci.bus_id"="0000:86:00",xid="48"} 1x20
  {__name__="nvidia_kmsg.xid.errors",entity_id="b",uuid="GPU-2",xid="13"} 4x20
`)
	t.Cleanup(func() { _ = st.Close() })
	runner := &engineRunner{t: t, engine: promqltest.NewTestEngine(t, false, 5*time.Minute, 50_000_000), storage: st,
		at: time.Unix(0, 0).UTC().Add(10 * time.Minute)}
	p := Planner{Renderer: testRenderer(t), Limits: ChunkLimits{MaxIDs: 255, MaxConcurrent: 1}, Runner: runner, Now: runner.now}
	got, err := p.Query(context.Background(), monitoring.QuerySpec{Metric: "nvidia_kmsg:xid.errors", Targets: targets("a")})
	if err != nil {
		t.Fatal(err)
	}
	byGPU := map[string]float64{}
	for _, s := range got {
		if s.Labels["entity_id"] != "a" {
			t.Errorf("series of another target: %v", s.Labels)
		}
		byGPU[s.Labels["uuid"]] = s.Points[len(s.Points)-1].V
	}
	if len(byGPU) != 2 || byGPU["GPU-1"] != 3 || byGPU[""] != 1 {
		t.Errorf("XID errors by GPU = %v, want GPU-1 3 (79 twice, 31 once) and 1 without a uuid", byGPU)
	}
}

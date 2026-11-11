package topprocessprocessor

import (
	"context"
	"os"
	"reflect"
	"sort"
	"testing"

	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/processor/processortest"
)

type fakeProc struct {
	pid    int64
	start  int64 // seconds
	cpuSec float64
	rss    int64
}

const sec = int64(1e9)

// batch builds one hostmetrics collection: a resource without process.pid
// and one resource per process.
func batch(ts int64, procs ...fakeProc) pmetric.Metrics {
	md := pmetric.NewMetrics()
	sys := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	sys.SetName("system.memory.usage")
	sys.SetEmptySum().DataPoints().AppendEmpty().SetIntValue(1)
	for _, p := range procs {
		rm := md.ResourceMetrics().AppendEmpty()
		rm.Resource().Attributes().PutInt(attrPID, p.pid)
		rm.Resource().Attributes().PutStr(attrExecutableName, "p")
		ms := rm.ScopeMetrics().AppendEmpty().Metrics()
		cpu := ms.AppendEmpty()
		cpu.SetName(metricCPUTime)
		cpuDps := cpu.SetEmptySum().DataPoints()
		for _, state := range []string{"user", "system"} {
			dp := cpuDps.AppendEmpty()
			dp.Attributes().PutStr("state", state)
			dp.SetStartTimestamp(pcommon.Timestamp(p.start * sec))
			dp.SetTimestamp(pcommon.Timestamp(ts * sec))
			dp.SetDoubleValue(p.cpuSec / 2)
		}
		mem := ms.AppendEmpty()
		mem.SetName(metricMemoryUsage)
		dp := mem.SetEmptySum().DataPoints().AppendEmpty()
		dp.SetStartTimestamp(pcommon.Timestamp(p.start * sec))
		dp.SetTimestamp(pcommon.Timestamp(ts * sec))
		dp.SetIntValue(p.rss)
	}
	return md
}

// keptPIDs returns the sorted process.pid of the resources left in md and
// checks that the one resource without process.pid is still there.
func keptPIDs(t *testing.T, md pmetric.Metrics) []int64 {
	t.Helper()
	var pids []int64
	others := 0
	rms := md.ResourceMetrics()
	for i := 0; i < rms.Len(); i++ {
		v, ok := rms.At(i).Resource().Attributes().Get(attrPID)
		if !ok {
			others++
			continue
		}
		pids = append(pids, v.Int())
	}
	if others != 1 {
		t.Fatalf("%d resources without process.pid left, want 1", others)
	}
	sort.Slice(pids, func(a, b int) bool { return pids[a] < pids[b] })
	return pids
}

func run(t *testing.T, p *topProcessProcessor, md pmetric.Metrics) []int64 {
	t.Helper()
	out, err := p.processMetrics(context.Background(), md)
	if err != nil {
		t.Fatal(err)
	}
	return keptPIDs(t, out)
}

func expect(t *testing.T, got []int64, want ...int64) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("kept pids %v, want %v", got, want)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		cfg Config
		ok  bool
	}{
		{Config{TopN: 10, By: []string{ByCPU, ByMemory}}, true},
		{Config{TopN: 1, By: []string{ByMemory}}, true},
		{Config{TopN: 0, By: []string{ByCPU}}, false},
		{Config{TopN: 10}, false},
		{Config{TopN: 10, By: []string{"disk"}}, false},
	}
	for _, c := range cases {
		if err := c.cfg.Validate(); (err == nil) != c.ok {
			t.Errorf("%+v: err=%v", c.cfg, err)
		}
	}
	def := createDefaultConfig().(*Config)
	if def.TopN != 10 || !reflect.DeepEqual(def.By, []string{ByCPU, ByMemory}) {
		t.Fatalf("default %+v", def)
	}
}

// Fewer processes than N all stay.
func TestFewerThanN(t *testing.T) {
	p := newProcessor(&Config{TopN: 10, By: []string{ByCPU, ByMemory}})
	expect(t, run(t, p, batch(100, fakeProc{1, 1, 5, 10}, fakeProc{2, 1, 5, 20}, fakeProc{3, 1, 5, 30})), 1, 2, 3)
}

// By CPU, the CPU time used since the previous batch ranks, not the total.
func TestByCPU(t *testing.T) {
	p := newProcessor(&Config{TopN: 2, By: []string{ByCPU}})
	run(t, p, batch(100,
		fakeProc{1, 1, 1000, 999}, fakeProc{2, 1, 10, 1}, fakeProc{3, 1, 10, 1}, fakeProc{4, 1, 10, 1}))
	// Over 10 s: 1 used 1 s, 2 used 5 s, 3 none, 4 used 20 s.
	expect(t, run(t, p, batch(110,
		fakeProc{1, 1, 1001, 999}, fakeProc{2, 1, 15, 1}, fakeProc{3, 1, 10, 1}, fakeProc{4, 1, 30, 1})), 2, 4)
}

// By memory, the larger process.memory.usage ranks first.
func TestByMemory(t *testing.T) {
	p := newProcessor(&Config{TopN: 2, By: []string{ByMemory}})
	expect(t, run(t, p, batch(100,
		fakeProc{1, 1, 9999, 10}, fakeProc{2, 1, 0, 300}, fakeProc{3, 1, 0, 200}, fakeProc{4, 1, 0, 100})), 2, 3)
}

// With both, the CPU top N and the memory top N are kept together; a process
// in both counts once.
func TestBothUnion(t *testing.T) {
	p := newProcessor(&Config{TopN: 2, By: []string{ByCPU, ByMemory}})
	run(t, p, batch(100,
		fakeProc{1, 1, 0, 1}, fakeProc{2, 1, 0, 1}, fakeProc{3, 1, 0, 500}, fakeProc{4, 1, 0, 400}, fakeProc{5, 1, 0, 1}))
	// CPU 1, 2; memory 3, 4: four.
	expect(t, run(t, p, batch(110,
		fakeProc{1, 1, 9, 1}, fakeProc{2, 1, 8, 1}, fakeProc{3, 1, 0, 500}, fakeProc{4, 1, 0, 400}, fakeProc{5, 1, 1, 1})), 1, 2, 3, 4)
	// CPU 3, 1; memory 3, 4: three.
	expect(t, run(t, p, batch(120,
		fakeProc{1, 1, 14, 1}, fakeProc{2, 1, 8, 1}, fakeProc{3, 1, 20, 500}, fakeProc{4, 1, 0, 400}, fakeProc{5, 1, 1, 1})), 1, 3, 4)
}

// Equal values keep the lower pid, whatever the order of the batch.
func TestTie(t *testing.T) {
	p := newProcessor(&Config{TopN: 2, By: []string{ByMemory}})
	expect(t, run(t, p, batch(100,
		fakeProc{9, 1, 0, 50}, fakeProc{7, 1, 0, 50}, fakeProc{8, 1, 0, 50}, fakeProc{3, 1, 0, 10})), 7, 8)
	// First place by value, the tie for second by pid.
	expect(t, run(t, p, batch(110,
		fakeProc{9, 1, 0, 50}, fakeProc{7, 1, 0, 50}, fakeProc{8, 1, 0, 50}, fakeProc{3, 1, 0, 90})), 3, 7)
}

// A process seen for the first time has CPU use 0; the same pid with another
// start time is another process.
func TestCPUFirstSeenAndPIDReuse(t *testing.T) {
	p := newProcessor(&Config{TopN: 1, By: []string{ByCPU}})
	expect(t, run(t, p, batch(100, fakeProc{5, 1, 500, 1}, fakeProc{2, 1, 1, 1})), 2)
	expect(t, run(t, p, batch(110, fakeProc{5, 1, 501, 1}, fakeProc{2, 1, 1, 1})), 5)
	// pid 2 now is a new process with 100 s of CPU time, first seen: 0.
	expect(t, run(t, p, batch(120, fakeProc{5, 1, 502, 1}, fakeProc{2, 115, 100, 1})), 5)
}

// A batch without processes passes through and keeps the previous CPU times.
func TestNonProcessBatchPassesThrough(t *testing.T) {
	p := newProcessor(&Config{TopN: 1, By: []string{ByCPU}})
	run(t, p, batch(100, fakeProc{5, 1, 500, 1}, fakeProc{2, 1, 1, 1}))
	expect(t, run(t, p, batch(105)), []int64(nil)...)
	expect(t, run(t, p, batch(110, fakeProc{5, 1, 501, 1}, fakeProc{2, 1, 1, 1})), 5)
}

// Kept processes carry process.executable.name and process.pid on every
// point; a point attribute already there is left alone, and the resource
// without process.pid gets nothing.
func TestCopyIdentityToPoints(t *testing.T) {
	p := newProcessor(&Config{TopN: 1, By: []string{ByMemory}})
	md := batch(100, fakeProc{1, 1, 0, 10}, fakeProc{2, 1, 0, 20})
	// pid 2's memory point already names the process.
	md.ResourceMetrics().At(2).ScopeMetrics().At(0).Metrics().At(1).Sum().DataPoints().At(0).
		Attributes().PutStr(attrExecutableName, "given")
	out, err := p.processMetrics(context.Background(), md)
	if err != nil {
		t.Fatal(err)
	}
	expect(t, keptPIDs(t, out), 2)
	rms := out.ResourceMetrics()
	for i := 0; i < rms.Len(); i++ {
		rm := rms.At(i)
		_, isProc := rm.Resource().Attributes().Get(attrPID)
		ms := rm.ScopeMetrics().At(0).Metrics()
		for j := 0; j < ms.Len(); j++ {
			m := ms.At(j)
			dps := m.Sum().DataPoints()
			for k := 0; k < dps.Len(); k++ {
				attrs := dps.At(k).Attributes()
				name, hasName := attrs.Get(attrExecutableName)
				pid, hasPID := attrs.Get(attrPID)
				if !isProc {
					if hasName || hasPID {
						t.Fatalf("%s without a process got %v", m.Name(), attrs.AsRaw())
					}
					continue
				}
				wantName := "p"
				if m.Name() == metricMemoryUsage {
					wantName = "given"
				}
				if !hasName || name.Str() != wantName || !hasPID || pid.Type() != pcommon.ValueTypeInt || pid.Int() != 2 {
					t.Fatalf("%s point attributes %v, want %s=%s and %s=2", m.Name(), attrs.AsRaw(), attrExecutableName, wantName, attrPID)
				}
			}
		}
	}
}

// Identity copy reaches every metric type, not only sums.
func TestCopyIdentityAllTypes(t *testing.T) {
	rm := pmetric.NewResourceMetrics()
	rm.Resource().Attributes().PutInt(attrPID, 7)
	rm.Resource().Attributes().PutStr(attrExecutableName, "x")
	ms := rm.ScopeMetrics().AppendEmpty().Metrics()
	ms.AppendEmpty().SetEmptyGauge().DataPoints().AppendEmpty()
	ms.AppendEmpty().SetEmptyHistogram().DataPoints().AppendEmpty()
	ms.AppendEmpty().SetEmptyExponentialHistogram().DataPoints().AppendEmpty()
	ms.AppendEmpty().SetEmptySummary().DataPoints().AppendEmpty()
	copyIdentity(rm)
	n := 0
	for j := 0; j < ms.Len(); j++ {
		forEachPointAttributes(ms.At(j), func(attrs pcommon.Map) {
			n++
			if v, _ := attrs.Get(attrExecutableName); v.Str() != "x" {
				t.Errorf("%v: name %q", ms.At(j).Type(), v.Str())
			}
			if v, _ := attrs.Get(attrPID); v.Int() != 7 {
				t.Errorf("%v: pid %d", ms.At(j).Type(), v.Int())
			}
		})
	}
	if n != 4 {
		t.Fatalf("visited %d points, want 4", n)
	}
}

func readJSON(t *testing.T, path string) pmetric.Metrics {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	md, err := (&pmetric.JSONUnmarshaler{}).UnmarshalMetrics(b)
	if err != nil {
		t.Fatal(err)
	}
	return md
}

// Two hostmetrics-shaped collections in testdata through the factory's
// processor, top 2 by both criteria:
//
//	1st: CPU all 0, so by pid 101, 202; memory 202 (2 GiB), 505 (800 MiB) -> 101, 202, 505
//	2nd: CPU 202 (2 cores), 404 (0.8 core); memory 202, 505 -> 202, 404, 505
func TestTestdata(t *testing.T) {
	f := NewFactory()
	if f.Type().String() != "top_process" {
		t.Errorf("type = %s", f.Type())
	}
	cfg := f.CreateDefaultConfig().(*Config)
	cfg.TopN = 2
	sink := new(consumertest.MetricsSink)
	proc, err := f.CreateMetrics(context.Background(), processortest.NewNopSettings(f.Type()), cfg, sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Start(context.Background(), componenttest.NewNopHost()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"testdata/scrape1.json", "testdata/scrape2.json"} {
		if err := proc.ConsumeMetrics(context.Background(), readJSON(t, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := proc.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := sink.AllMetrics()
	if len(got) != 2 {
		t.Fatalf("%d batches sent, want 2", len(got))
	}
	expect(t, keptPIDs(t, got[0]), 101, 202, 505)
	expect(t, keptPIDs(t, got[1]), 202, 404, 505)

	// Kept processes keep their three metrics and resource attributes, and
	// their points gain the name and pid.
	names := map[int64]string{202: "java", 404: "python3", 505: "nginx"}
	rms := got[1].ResourceMetrics()
	for i := 0; i < rms.Len(); i++ {
		rm := rms.At(i)
		pid, ok := rm.Resource().Attributes().Get(attrPID)
		if !ok {
			continue
		}
		ms := rm.ScopeMetrics().At(0).Metrics()
		if ms.Len() != 3 {
			t.Fatalf("pid %d: %d metrics, want 3", pid.Int(), ms.Len())
		}
		if v, ok := rm.Resource().Attributes().Get(attrExecutableName); !ok || v.Str() != names[pid.Int()] {
			t.Fatalf("pid %d: resource name %q, want %q", pid.Int(), v.Str(), names[pid.Int()])
		}
		for j := 0; j < ms.Len(); j++ {
			forEachPointAttributes(ms.At(j), func(attrs pcommon.Map) {
				n, _ := attrs.Get(attrExecutableName)
				p, _ := attrs.Get(attrPID)
				if n.Str() != names[pid.Int()] || p.Int() != pid.Int() {
					t.Errorf("pid %d %s: point attributes %v", pid.Int(), ms.At(j).Name(), attrs.AsRaw())
				}
			})
		}
	}
}

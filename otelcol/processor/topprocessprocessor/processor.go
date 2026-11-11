package topprocessprocessor

import (
	"context"
	"sort"
	"sync"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

const (
	attrPID            = "process.pid"
	attrExecutableName = "process.executable.name"
	metricCPUTime      = "process.cpu.time"
	metricMemoryUsage  = "process.memory.usage"
)

// procKey tells processes apart across batches. The start time is part of it
// so a pid the kernel hands to a new process does not inherit the old one's
// CPU time.
type procKey struct {
	pid   int64
	start pcommon.Timestamp
}

// cpuSample is a process's cumulative CPU seconds at a time.
type cpuSample struct {
	total float64
	ts    pcommon.Timestamp
}

// candidate is a process resource of the batch with its ranking values.
type candidate struct {
	idx int
	pid int64
	cpu float64
	mem float64
}

type topProcessProcessor struct {
	cfg  *Config
	mu   sync.Mutex
	last map[procKey]cpuSample
}

func newProcessor(cfg *Config) *topProcessProcessor {
	return &topProcessProcessor{cfg: cfg, last: make(map[procKey]cpuSample)}
}

// processMetrics ranks the process resources of md, drops those no criterion
// keeps and labels the points of those it keeps (see copyIdentity).
func (p *topProcessProcessor) processMetrics(_ context.Context, md pmetric.Metrics) (pmetric.Metrics, error) {
	rms := md.ResourceMetrics()
	isProc := make([]bool, rms.Len())
	var cands []candidate

	p.mu.Lock()
	seen := make(map[procKey]cpuSample)
	for i := 0; i < rms.Len(); i++ {
		rm := rms.At(i)
		v, ok := rm.Resource().Attributes().Get(attrPID)
		if !ok || v.Type() != pcommon.ValueTypeInt {
			continue
		}
		isProc[i] = true
		c := candidate{idx: i, pid: v.Int()}
		if s, start, ok := cpuTime(rm); ok {
			key := procKey{pid: c.pid, start: start}
			// CPU use is the CPU time used since the previous batch per
			// second elapsed; a process seen for the first time ranks at 0.
			if prev, ok := p.last[key]; ok && s.ts > prev.ts && s.total >= prev.total {
				elapsed := float64(s.ts-prev.ts) / 1e9
				c.cpu = (s.total - prev.total) / elapsed
			}
			seen[key] = s
		}
		c.mem = memoryUsage(rm)
		cands = append(cands, c)
	}
	// A batch without processes (another receiver's) keeps the previous
	// CPU times for the next process batch.
	if len(cands) > 0 {
		p.last = seen
	}
	p.mu.Unlock()

	keep := p.selectTop(cands, rms.Len())
	i := -1
	rms.RemoveIf(func(rm pmetric.ResourceMetrics) bool {
		i++
		if !isProc[i] {
			return false
		}
		if !keep[i] {
			return true
		}
		copyIdentity(rm)
		return false
	})
	return md, nil
}

// selectTop returns, by resource index, the union of the top TopN of every
// criterion. Equal values rank the lower pid first, so the choice does not
// depend on the order of the batch.
func (p *topProcessProcessor) selectTop(cands []candidate, n int) []bool {
	keep := make([]bool, n)
	sorted := make([]candidate, len(cands))
	for _, by := range p.cfg.By {
		copy(sorted, cands)
		val := func(c candidate) float64 { return c.cpu }
		if by == ByMemory {
			val = func(c candidate) float64 { return c.mem }
		}
		sort.SliceStable(sorted, func(a, b int) bool {
			va, vb := val(sorted[a]), val(sorted[b])
			if va != vb {
				return va > vb
			}
			return sorted[a].pid < sorted[b].pid
		})
		for _, c := range sorted[:min(p.cfg.TopN, len(sorted))] {
			keep[c.idx] = true
		}
	}
	return keep
}

// copyIdentity copies the resource's process.executable.name and process.pid
// onto every data point of rm that does not have them yet (see the package
// documentation for why).
func copyIdentity(rm pmetric.ResourceMetrics) {
	res := rm.Resource().Attributes()
	var copies []string
	for _, k := range []string{attrExecutableName, attrPID} {
		if _, ok := res.Get(k); ok {
			copies = append(copies, k)
		}
	}
	if len(copies) == 0 {
		return
	}
	label := func(attrs pcommon.Map) {
		for _, k := range copies {
			if _, ok := attrs.Get(k); ok {
				continue
			}
			v, _ := res.Get(k)
			v.CopyTo(attrs.PutEmpty(k))
		}
	}
	sms := rm.ScopeMetrics()
	for i := 0; i < sms.Len(); i++ {
		ms := sms.At(i).Metrics()
		for j := 0; j < ms.Len(); j++ {
			forEachPointAttributes(ms.At(j), label)
		}
	}
}

// forEachPointAttributes calls fn with the attributes of every data point of
// m, whatever its type.
func forEachPointAttributes(m pmetric.Metric, fn func(pcommon.Map)) {
	switch m.Type() {
	case pmetric.MetricTypeGauge:
		dps := m.Gauge().DataPoints()
		for k := 0; k < dps.Len(); k++ {
			fn(dps.At(k).Attributes())
		}
	case pmetric.MetricTypeSum:
		dps := m.Sum().DataPoints()
		for k := 0; k < dps.Len(); k++ {
			fn(dps.At(k).Attributes())
		}
	case pmetric.MetricTypeHistogram:
		dps := m.Histogram().DataPoints()
		for k := 0; k < dps.Len(); k++ {
			fn(dps.At(k).Attributes())
		}
	case pmetric.MetricTypeExponentialHistogram:
		dps := m.ExponentialHistogram().DataPoints()
		for k := 0; k < dps.Len(); k++ {
			fn(dps.At(k).Attributes())
		}
	case pmetric.MetricTypeSummary:
		dps := m.Summary().DataPoints()
		for k := 0; k < dps.Len(); k++ {
			fn(dps.At(k).Attributes())
		}
	}
}

// cpuTime returns the cumulative CPU seconds of process.cpu.time over all its
// states, the latest point time and the process's start time.
func cpuTime(rm pmetric.ResourceMetrics) (cpuSample, pcommon.Timestamp, bool) {
	var s cpuSample
	var start pcommon.Timestamp
	found := false
	forEachNumberPoint(rm, metricCPUTime, func(dp pmetric.NumberDataPoint) {
		s.total += pointValue(dp)
		s.ts = max(s.ts, dp.Timestamp())
		start = dp.StartTimestamp()
		found = true
	})
	return s, start, found
}

// memoryUsage returns process.memory.usage, the resident memory in bytes.
func memoryUsage(rm pmetric.ResourceMetrics) float64 {
	var v float64
	forEachNumberPoint(rm, metricMemoryUsage, func(dp pmetric.NumberDataPoint) {
		v = pointValue(dp)
	})
	return v
}

func forEachNumberPoint(rm pmetric.ResourceMetrics, name string, fn func(pmetric.NumberDataPoint)) {
	sms := rm.ScopeMetrics()
	for i := 0; i < sms.Len(); i++ {
		ms := sms.At(i).Metrics()
		for j := 0; j < ms.Len(); j++ {
			m := ms.At(j)
			if m.Name() != name {
				continue
			}
			var dps pmetric.NumberDataPointSlice
			switch m.Type() {
			case pmetric.MetricTypeSum:
				dps = m.Sum().DataPoints()
			case pmetric.MetricTypeGauge:
				dps = m.Gauge().DataPoints()
			default:
				continue
			}
			for k := 0; k < dps.Len(); k++ {
				fn(dps.At(k))
			}
		}
	}
}

func pointValue(dp pmetric.NumberDataPoint) float64 {
	if dp.ValueType() == pmetric.NumberDataPointValueTypeInt {
		return float64(dp.IntValue())
	}
	return dp.DoubleValue()
}

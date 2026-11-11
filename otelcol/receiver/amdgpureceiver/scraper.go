package amdgpureceiver

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

// gpuScraper turns a source's samples into gauges named namespace.field.
type gpuScraper struct {
	source Source
	now    func() time.Time
}

func (s *gpuScraper) scrape(ctx context.Context) (pmetric.Metrics, error) {
	md := pmetric.NewMetrics()
	gpus, err := s.source.Read(ctx)
	if err != nil {
		return md, err
	}
	ts := pcommon.NewTimestampFromTime(s.now())
	sm := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty()
	sm.Scope().SetName("github.com/cloud-barista/ai-o11y/otelcol/receiver/amdgpureceiver")
	byName := map[string]pmetric.Gauge{}
	for _, g := range gpus {
		for _, v := range g.Values {
			name := s.source.Namespace() + "." + v.Field
			gauge, ok := byName[name]
			if !ok {
				m := sm.Metrics().AppendEmpty()
				m.SetName(name)
				m.SetUnit(v.Unit)
				gauge = m.SetEmptyGauge()
				byName[name] = gauge
			}
			dp := gauge.DataPoints().AppendEmpty()
			dp.SetTimestamp(ts)
			dp.SetDoubleValue(v.Value)
			for k, a := range g.Attributes {
				dp.Attributes().PutStr(k, a)
			}
		}
	}
	return md, nil
}

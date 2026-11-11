package amdgpureceiver

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/receiver/receivertest"
)

// TestScrapeRecordedV710 scrapes answers recorded on an Azure V710 VM end to
// end: the
// metrics are named amd_smi.<path> and carry the GPU's index, uuid and name.
func TestScrapeRecordedV710(t *testing.T) {
	at := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	src, _ := fakeSMI(t, "static-asic-v710-recorded.json", "list-v710-recorded.json", "metric-v710-recorded.json")
	s := &gpuScraper{source: src, now: func() time.Time { return at }}
	md, err := s.scrape(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	metrics := md.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics()
	var used pmetric.Metric
	for i := range metrics.Len() {
		if metrics.At(i).Name() == "amd_smi.mem_usage.used_vram" {
			used = metrics.At(i)
		}
	}
	if used.Type() != pmetric.MetricTypeGauge || used.Unit() != "MiBy" || used.Gauge().DataPoints().Len() != 1 {
		t.Fatalf("amd_smi.mem_usage.used_vram = %s %v in %d metrics", used.Unit(), used.Type(), metrics.Len())
	}
	dp := used.Gauge().DataPoints().At(0)
	if dp.DoubleValue() != 227 || dp.Timestamp().AsTime() != at {
		t.Errorf("data point = %v at %v", dp.DoubleValue(), dp.Timestamp().AsTime())
	}
	for k, want := range map[string]string{"index": "0", "uuid": "46007461-0000-1000-80c0-38ba93f59d90", "name": "AMD Radeon Pro V710 MxGPU"} {
		if v, _ := dp.Attributes().Get(k); v.Str() != want {
			t.Errorf("%s = %q, want %q", k, v.Str(), want)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	cfg := createDefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"unknown source": func(c *Config) { c.Source = "rocm_smi" },
		"no path":        func(c *Config) { c.AMDSMI.Path = "" },
		"no interval":    func(c *Config) { c.CollectionInterval = 0 },
	} {
		c := createDefaultConfig()
		mutate(c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate succeeded", name)
		}
	}
}

func TestFactoryStartsAndStops(t *testing.T) {
	f := NewFactory()
	if f.Type().String() != "amd_gpu" {
		t.Errorf("type = %s", f.Type())
	}
	r, err := f.CreateMetrics(context.Background(), receivertest.NewNopSettings(f.Type()), f.CreateDefaultConfig(), new(consumertest.MetricsSink))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background(), componenttest.NewNopHost()); err != nil {
		t.Fatal(err)
	}
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

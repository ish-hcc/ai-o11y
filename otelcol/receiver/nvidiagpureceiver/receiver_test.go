package nvidiagpureceiver

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/receiver/receivertest"
)

type fixedSource struct{ gpus []GPU }

func (fixedSource) Namespace() string                     { return SourceNvidiaSMI }
func (f fixedSource) Read(context.Context) ([]GPU, error) { return f.gpus, nil }

func TestScrapeNamesMetricsAfterTheSource(t *testing.T) {
	at := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	s := &gpuScraper{now: func() time.Time { return at }, source: fixedSource{gpus: []GPU{
		{Attributes: map[string]string{"index": "0", "uuid": "GPU-a", "name": "T4"},
			Values: []Value{{Field: "utilization.gpu", Unit: "%", Value: 40}, {Field: "memory.used", Unit: "MiBy", Value: 512}}},
		{Attributes: map[string]string{"index": "1", "uuid": "GPU-b", "name": "T4"},
			Values: []Value{{Field: "utilization.gpu", Unit: "%", Value: 60}}},
	}}}
	md, err := s.scrape(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	metrics := md.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics()
	if metrics.Len() != 2 {
		t.Fatalf("metrics = %d", metrics.Len())
	}
	util := metrics.At(0)
	if util.Name() != "nvidia_smi.utilization.gpu" || util.Unit() != "%" || util.Type() != pmetric.MetricTypeGauge ||
		util.Gauge().DataPoints().Len() != 2 {
		t.Fatalf("first metric = %s %s %v", util.Name(), util.Unit(), util.Type())
	}
	dp := util.Gauge().DataPoints().At(1)
	if dp.DoubleValue() != 60 || dp.Timestamp().AsTime() != at {
		t.Errorf("data point = %v at %v", dp.DoubleValue(), dp.Timestamp().AsTime())
	}
	if uuid, _ := dp.Attributes().Get("uuid"); uuid.Str() != "GPU-b" {
		t.Errorf("uuid = %v", uuid.Str())
	}
}

func TestConfigValidate(t *testing.T) {
	cfg := createDefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config: %v", err)
	}
	if cfg.XID.Enabled || cfg.XID.Path != "/dev/kmsg" {
		t.Errorf("default xid = %+v, want off, reading /dev/kmsg", cfg.XID)
	}
	for name, mutate := range map[string]func(*Config){
		"unknown source": func(c *Config) { c.Source = "nvml" },
		"no path":        func(c *Config) { c.NvidiaSMI.Path = "" },
		"no interval":    func(c *Config) { c.CollectionInterval = 0 },
		"xid no path":    func(c *Config) { c.XID = XIDConfig{Enabled: true} },
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
	if f.Type().String() != "nvidia_gpu" {
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
